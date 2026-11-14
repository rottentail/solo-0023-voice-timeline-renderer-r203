package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

// OpenSourceReader opens a streaming PCM reader over the stored source file.
// The file is validated as PCM WAV up front; the renderer never loads the whole
// payload, so long projects stay inside a small working set.
func (repo *Repository) OpenSourceReader(_ context.Context, file render.SourceFile) (audiocodec.FrameReader, error) {
	handle, err := os.Open(file.Path)
	if err != nil {
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot open source media", err)
	}
	info, err := handle.Stat()
	if err != nil {
		_ = handle.Close()
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot stat source media", err)
	}
	reader, err := audiocodec.NewFileFrameReader(handle, info.Size())
	if err != nil {
		_ = handle.Close()
		return nil, domain.Wrap(domain.KindInvalid, "invalid_wav", "", "stored source media is not a valid PCM WAV", err)
	}
	return &closingFrameReader{FrameReader: reader, closer: handle}, nil
}

// DecodeSourceAsset resolves the immutable identity of one source for the
// renderer without loading its bytes.
func (repo *Repository) DecodeSourceAsset(id string) (render.SourceFile, error) {
	asset, err := repo.GetSourceMeta(id)
	if err != nil {
		return render.SourceFile{}, err
	}
	return render.SourceFile{Asset: asset, Path: repo.path("sources", id, "source.wav")}, nil
}

type closingFrameReader struct {
	audiocodec.FrameReader
	closer io.Closer
}

func (reader *closingFrameReader) Close() error {
	closeErr := reader.FrameReader.Close()
	return errors.Join(closeErr, reader.closer.Close())
}

// RenderPartialPath returns the durable path for an in-progress render WAV.
// It lives outside the staging directory so an interrupted render can resume
// from its last checkpoint instead of restarting from scratch.
func (repo *Repository) RenderPartialPath(renderID string) (string, error) {
	return repo.path("tmp", "partial-"+renderID+".wav"), nil
}

// RemoveRenderPartial deletes an in-progress render WAV after publication or a
// terminal failure.
func (repo *Repository) RemoveRenderPartial(renderID string) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	path := repo.path("tmp", "partial-"+renderID+".wav")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot remove partial render", err)
	}
	return nil
}

// RenderStagingDir creates a render scratch directory under tmp. The caller
// owns it and must remove it on failure.
func (repo *Repository) RenderStagingDir(renderID string) (string, error) {
	directory, err := os.MkdirTemp(repo.path("tmp"), "render-"+renderID+"-*")
	if err != nil {
		return "", domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create render staging directory", err)
	}
	return directory, nil
}

// SaveRenderCheckpoint persists a render checkpoint for crash-safe resume.
func (repo *Repository) SaveRenderCheckpoint(renderID string, checkpoint render.RenderCheckpoint) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	path := repo.path("jobs", renderID+".checkpoint.json")
	if err := writeJSONAtomic(path, checkpoint, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render checkpoint", err)
	}
	return nil
}

// LoadRenderCheckpoint returns a stored checkpoint when one exists.
func (repo *Repository) LoadRenderCheckpoint(renderID string) (*render.RenderCheckpoint, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var checkpoint render.RenderCheckpoint
	if err := readJSON(repo.path("jobs", renderID+".checkpoint.json"), &checkpoint); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render checkpoint", err)
	}
	return &checkpoint, nil
}

// ClearRenderCheckpoint removes a checkpoint after the published artifacts are
// committed, so a completed render can never resume into stale state.
func (repo *Repository) ClearRenderCheckpoint(renderID string) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	path := repo.path("jobs", renderID+".checkpoint.json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot remove render checkpoint", err)
	}
	return nil
}

// CommitRenderFromFile publishes completed output.wav and receipt.json by
// moving the staged directory into place. A completed render is never
// overwritten by a retry.
func (repo *Repository) CommitRenderFromFile(stagingDir string, renderID string, receipt domain.RenderReceipt, wavPath string) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	final := repo.path("renders", renderID)
	if _, err := os.Stat(final); err == nil {
		return domain.E(domain.KindConflict, "render_conflict", "render_id", "render artifacts already exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect render destination", err)
	}
	if err := os.Rename(wavPath, filepath.Join(stagingDir, "output.wav")); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot stage render output", err)
	}
	if err := writeJSONAtomic(filepath.Join(stagingDir, "receipt.json"), receipt, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render receipt", err)
	}
	if err := os.Rename(stagingDir, final); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot commit render artifacts", err)
	}
	if err := syncDirectory(repo.path("renders")); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync render index", err)
	}
	return nil
}

// ReadRenderReceiptFile reads the published receipt without loading output.
func (repo *Repository) ReadRenderReceiptFile(renderID string) (domain.RenderReceipt, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var receipt domain.RenderReceipt
	if err := readJSON(repo.path("renders", renderID, "receipt.json"), &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is not available")
		}
		return domain.RenderReceipt{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render receipt", err)
	}
	return receipt, nil
}
