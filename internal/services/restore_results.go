package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Kept per-model restoration results.
//
// The tarball is the download artifact, but the comparison viewer wants to play
// each model's MP4 directly. After the tarball is packed the stitched files and
// the reference clip are moved out of stage/ (which the deferred cleanup
// deletes) into <outputDir>/<jobID>/results/<modelID>/<file>.mp4, next to a
// copy of the manifest. They live inside the job directory, so the retention
// worker reaps them together with the tarball.

const (
	restoreKeptDirName      = "results"
	restoreKeptManifestName = "manifest.json"
	restoreOriginalID       = "original"
	restoreOriginalFileName = "clip.mp4"
	restoreResultMime       = "video/mp4"
)

// moveRestoreFile moves src to dst: rename (same filesystem, free), then a
// hardlink, then a plain copy as a last resort.
func moveRestoreFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyRestoreFileSafe(src, dst)
}

func copyRestoreFileSafe(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// keepRestoreResults moves the completed models' MP4s and the reference clip
// out of stageRoot into jobOutputDir/results and writes results/manifest.json.
// Model files that are missing are skipped (the manifest still lists them; the
// results reader will not offer them).
func keepRestoreResults(stageRoot, jobOutputDir string, manifest restoreManifest) error {
	keptRoot := filepath.Join(jobOutputDir, restoreKeptDirName)
	if err := os.MkdirAll(keptRoot, 0o755); err != nil {
		return err
	}
	srcOriginal := filepath.Join(stageRoot, restoreResultBaseName, restoreOriginalID, restoreOriginalFileName)
	if err := moveRestoreFile(srcOriginal, filepath.Join(keptRoot, restoreOriginalID, restoreOriginalFileName)); err != nil {
		return fmt.Errorf("keep original clip: %w", err)
	}
	for _, m := range manifest.Models {
		if m.Status != "completed" || m.OutputFile == "" || !models.IsRestoreModelID(m.ID) {
			continue
		}
		base := filepath.Base(m.OutputFile)
		src := filepath.Join(stageRoot, filepath.FromSlash(m.OutputFile))
		if err := moveRestoreFile(src, filepath.Join(keptRoot, m.ID, base)); err != nil {
			return fmt.Errorf("keep %s result: %w", m.ID, err)
		}
	}
	return writeJSON(filepath.Join(keptRoot, restoreKeptManifestName), manifest)
}

func readKeptRestoreManifest(outputRoot, jobID string) (restoreManifest, string, error) {
	var m restoreManifest
	if jobID == "" || strings.ContainsAny(jobID, `/\`) || jobID == "." || jobID == ".." {
		return m, "", errors.New("invalid job id")
	}
	keptRoot := filepath.Join(outputRoot, jobID, restoreKeptDirName)
	raw, err := os.ReadFile(filepath.Join(keptRoot, restoreKeptManifestName))
	if err != nil {
		return m, "", err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, "", err
	}
	return m, keptRoot, nil
}

// ReadRestoreResults builds the results listing from the kept manifest.
func ReadRestoreResults(outputRoot, jobID string) (*models.RestoreResultsResponse, error) {
	m, keptRoot, err := readKeptRestoreManifest(outputRoot, jobID)
	if err != nil {
		return nil, err
	}
	resp := &models.RestoreResultsResponse{
		JobID: jobID,
		Original: models.RestoreResultOriginal{
			ID: restoreOriginalID, Label: "Original",
			Width: m.Source.Width, Height: m.Source.Height,
			FileName: restoreOriginalFileName, Mime: restoreResultMime,
		},
		Models: []models.RestoreResultModel{},
	}
	if st, err := os.Stat(filepath.Join(keptRoot, restoreOriginalID, restoreOriginalFileName)); err == nil {
		resp.Original.SizeBytes = st.Size()
	} else {
		return nil, err
	}
	for _, o := range m.Models {
		item := models.RestoreResultModel{
			ID: o.ID, Group: o.Group, Status: o.Status,
			DurationSeconds: o.DurationSeconds, Note: o.Note, Error: o.Error,
		}
		if o.Status == "completed" {
			if _, p, err := resolveKeptModel(m, keptRoot, o.ID); err == nil {
				if st, err := os.Stat(p); err == nil {
					item.FileName = filepath.Base(p)
					item.SizeBytes = st.Size()
					item.Mime = restoreResultMime
					item.Width, item.Height = o.OutputWidth, o.OutputHeight
				}
			}
		}
		resp.Models = append(resp.Models, item)
	}
	return resp, nil
}

func resolveKeptModel(m restoreManifest, keptRoot, name string) (restoreModelOutcome, string, error) {
	for _, o := range m.Models {
		if o.ID == name && o.Status == "completed" && o.OutputFile != "" && models.IsRestoreModelID(o.ID) {
			return o, filepath.Join(keptRoot, o.ID, filepath.Base(o.OutputFile)), nil
		}
	}
	return restoreModelOutcome{}, "", errors.New("not found")
}

// RestoreResultPath resolves one result name ("original" or a completed model
// id present in the manifest) to its file. The name is only ever compared
// against the manifest; user input is never joined into a path.
func RestoreResultPath(outputRoot, jobID, name string) (string, error) {
	m, keptRoot, err := readKeptRestoreManifest(outputRoot, jobID)
	if err != nil {
		return "", err
	}
	if name == restoreOriginalID {
		return filepath.Join(keptRoot, restoreOriginalID, restoreOriginalFileName), nil
	}
	_, p, err := resolveKeptModel(m, keptRoot, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}
