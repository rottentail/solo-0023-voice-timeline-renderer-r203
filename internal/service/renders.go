package service

import (
	"context"
	"errors"
	"os"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

// CreateRender is idempotent per render identifier. A completed render is
// returned untouched. An interrupted render resumes from its durable
// checkpoint when the selection is unchanged, and otherwise restarts with
// fresh error history that cannot borrow from another source selection.
func (s *Service) CreateRender(
	ctx context.Context,
	id string,
	request RenderWriteRequest,
) (domain.RenderJob, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.RenderJob{}, false, err
	}
	if err := domain.ValidateResourceID("render_id", id); err != nil {
		return domain.RenderJob{}, false, err
	}
	if err := domain.ValidateResourceID("timeline_id", request.TimelineID); err != nil {
		return domain.RenderJob{}, false, err
	}

	existing, getErr := s.repo.GetJob(id)
	created := false
	var job domain.RenderJob
	if getErr == nil {
		if existing.TimelineID != request.TimelineID {
			return domain.RenderJob{}, false, domain.E(domain.KindConflict, "render_conflict", "render_id", "identifier is already bound to a different timeline")
		}
		if existing.State == domain.RenderCompleted {
			// A completed receipt and output are immutable: a retry never
			// rewrites either artifact.
			return existing, false, nil
		}
		if existing.State == domain.RenderFailed {
			// Failed is terminal as well. Surface the stored failure rather
			// than attempting another transition.
			if existing.Error != nil {
				return existing, false, domain.E(domain.KindConflict, existing.Error.Code, existing.Error.Field, existing.Error.Message)
			}
			return existing, false, domain.E(domain.KindConflict, "render_failed", "render_id", "render job already failed")
		}
		job = existing
	} else if appErr := domain.AsAppError(getErr); appErr.Kind != domain.KindNotFound {
		return domain.RenderJob{}, false, getErr
	} else {
		now := time.Now().UTC()
		job = domain.RenderJob{
			ID:         id,
			TimelineID: request.TimelineID,
			State:      domain.RenderQueued,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := s.repo.SaveJob(job); err != nil {
			return domain.RenderJob{}, false, err
		}
		created = true
	}

	plan, err := s.repo.GetTimeline(request.TimelineID)
	if err != nil {
		return failRenderState(s, job, err, created)
	}
	mode, err := audiocodec.ParseQuantizationMode(plan.Quantization)
	if err != nil {
		return failRenderState(s, job, domain.Wrap(domain.KindInvalid, "invalid_quantization", "quantization", "timeline quantization mode is not supported", err), created)
	}

	sourceFiles := make(map[string]render.SourceFile)
	digests := make(map[string]string)
	for _, span := range plan.Spans {
		if _, loaded := sourceFiles[span.SourceID]; loaded {
			continue
		}
		file, loadErr := s.repo.DecodeSourceAsset(span.SourceID)
		if loadErr != nil {
			return failRenderState(s, job, loadErr, created)
		}
		sourceFiles[span.SourceID] = file
		digests[span.SourceID] = file.Asset.Digest
	}

	seed := audiocodec.QuantizerSeed(id)
	selection := render.SelectionDigest(id, plan, digests, mode, seed)
	checkpoint, err := s.loadCheckpoint(id, selection, mode, plan)
	if err != nil {
		return failRenderState(s, job, err, created)
	}
	if checkpoint != nil {
		// A checkpoint is only usable if its partial WAV still covers the
		// checkpoint frame. If the partial is missing or truncated, discard the
		// checkpoint and render from the start; a clean restart is deterministic
		// and still yields the same PCM as an uninterrupted render.
		partialPath, pathErr := s.repo.RenderPartialPath(id)
		if pathErr != nil || !partialCovers(partialPath, checkpoint.FrameIndex, plan.OutputChannels) {
			_ = s.repo.ClearRenderCheckpoint(id)
			_ = s.repo.RemoveRenderPartial(id)
			checkpoint = nil
		}
	}

	if job.State != domain.RenderRendering && job.State != domain.RenderFinalizing {
		if err := s.advanceJob(&job, domain.RenderRendering, nil); err != nil {
			return job, created, err
		}
	}

	staging, err := s.repo.RenderStagingDir(id)
	if err != nil {
		return failRenderState(s, job, err, created)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(staging)
		}
	}()

	provider := &repositoryAudioSource{repo: s.repo, files: sourceFiles}
	// The unreceived partial WAV lives beside the checkpoint, not in staging,
	// so an interrupted render can continue from its exact error history.
	outputPath, err := s.repo.RenderPartialPath(id)
	if err != nil {
		return failRenderState(s, job, err, created)
	}
	checkpointSink := func(saved render.RenderCheckpoint) error {
		return s.repo.SaveRenderCheckpoint(id, saved)
	}
	result, renderErr := render.RenderStream(
		ctx,
		id,
		plan,
		render.QuantizationRequest{
			Mode:            mode,
			Seed:            seed,
			SelectionDigest: selection,
			Checkpoint:      checkpoint,
		},
		provider,
		render.StreamLimits{MaxOutputFrames: s.config.MaxOutputFrames},
		outputPath,
		checkpointSink,
	)
	if renderErr != nil {
		if isInterruption(renderErr) {
			// Keep the job active and the checkpoint and partial WAV on disk so
			// the same identifier resumes with identical PCM instead of
			// borrowing or discarding the error history.
			if err := s.repo.SaveJob(job); err != nil {
				return job, created, err
			}
			return job, created, domain.Wrap(domain.KindUnready, "render_interrupted", "", "render was interrupted and can be retried", renderErr)
		}
		_ = s.repo.RemoveRenderPartial(id)
		return failRenderState(s, job, domain.Wrap(domain.KindInternal, "render_failed", "", "timeline rendering failed", renderErr), created)
	}
	if err := s.advanceJob(&job, domain.RenderFinalizing, nil); err != nil {
		return job, created, err
	}
	if err := s.repo.CommitRenderFromFile(staging, id, result.Receipt, result.OutputPath); err != nil {
		return failRenderState(s, job, err, created)
	}
	cleanupStaging = false
	if err := s.repo.ClearRenderCheckpoint(id); err != nil {
		return job, created, err
	}
	_ = s.repo.RemoveRenderPartial(id)
	output := result.Output
	job.Output = &output
	job.ReceiptDigest = receiptDigest(result.Receipt)
	if err := s.advanceJob(&job, domain.RenderCompleted, nil); err != nil {
		return job, created, err
	}
	return job, created, nil
}

// loadCheckpoint returns a usable checkpoint or nil. A checkpoint whose
// selection digest or quantization spec differs is discarded: continuing from
// it would silently import error history from another selection.
func (s *Service) loadCheckpoint(id, selection string, mode audiocodec.QuantizationMode, plan domain.Timeline) (*render.RenderCheckpoint, error) {
	checkpoint, err := s.repo.LoadRenderCheckpoint(id)
	if err != nil {
		return nil, err
	}
	if checkpoint == nil {
		return nil, nil
	}
	spec, err := audiocodec.BuildQuantizationSpec(mode, plan.SampleRate, plan.OutputChannels)
	if err != nil {
		return nil, err
	}
	if checkpoint.SelectionDigest != selection || checkpoint.QuantizerDigest != spec.Digest() {
		_ = s.repo.ClearRenderCheckpoint(id)
		return nil, nil
	}
	return checkpoint, nil
}

func isInterruption(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

type repositoryAudioSource struct {
	repo  Repository
	files map[string]render.SourceFile
}

func (source *repositoryAudioSource) OpenSource(_ context.Context, sourceID string) (render.SourceFile, error) {
	file, ok := source.files[sourceID]
	if !ok {
		return render.SourceFile{}, domain.E(domain.KindNotFound, "source_not_found", "source_id", "source does not exist")
	}
	return file, nil
}

func (source *repositoryAudioSource) OpenReader(ctx context.Context, file render.SourceFile) (audiocodec.FrameReader, error) {
	return source.repo.OpenSourceReader(ctx, file)
}

func receiptDigest(receipt domain.RenderReceipt) string {
	return receipt.Output.Digest
}

func (s *Service) advanceJob(job *domain.RenderJob, state domain.RenderState, detail *domain.ErrorDetail) error {
	if err := job.Transition(state, detail, time.Now().UTC()); err != nil {
		return err
	}
	if err := s.repo.SaveJob(*job); err != nil {
		return err
	}
	return nil
}

func failRenderState(s *Service, job domain.RenderJob, cause error, created bool) (domain.RenderJob, bool, error) {
	if job.State == domain.RenderCompleted || job.State == domain.RenderFailed {
		return job, created, cause
	}
	appErr := domain.AsAppError(cause)
	detail := &domain.ErrorDetail{
		Code:    appErr.Detail.Code,
		Message: appErr.Detail.Message,
		Field:   appErr.Detail.Field,
	}
	if err := job.Transition(domain.RenderFailed, detail, time.Now().UTC()); err != nil {
		return job, created, err
	}
	job.Output = nil
	job.ReceiptDigest = ""
	if err := s.repo.SaveJob(job); err != nil {
		return job, created, err
	}
	return job, created, appErr
}

func partialCovers(path string, frames int64, channels int) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	required := int64(44) + frames*int64(channels)*2
	return info.Size() >= required
}
