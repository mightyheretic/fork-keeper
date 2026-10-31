package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// Release file statuses.
const (
	ReleaseFileUploaded = "uploaded"
	ReleaseFileFailed   = "failed"
)

// ReleaseFile is an upstream release file copied to the fork's release.
type ReleaseFile struct {
	Provider      string
	Repo          string
	Tag           string
	Name          string
	Platform      string
	Size          int64
	SHA256        string
	Fork          string
	ForkReleaseID int64
	ForkAssetID   int64
	Status        string
	Error         string
	UpdatedAt     time.Time
}

// SaveReleaseFile records the outcome of copying one file.
func (s *Store) SaveReleaseFile(ctx context.Context, f ReleaseFile) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO release_files (provider, repo, tag, name, platform, size, sha256, fork, fork_release_id,
			fork_asset_id, status, error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo, tag, name) DO UPDATE SET
			platform = excluded.platform, size = excluded.size, sha256 = excluded.sha256, fork = excluded.fork,
			fork_release_id = excluded.fork_release_id, fork_asset_id = excluded.fork_asset_id,
			status = excluded.status, error = excluded.error, updated_at = excluded.updated_at`,
		f.Provider, f.Repo, f.Tag, f.Name, f.Platform, f.Size, f.SHA256, f.Fork, f.ForkReleaseID, f.ForkAssetID,
		f.Status, f.Error, s.nowMillis())
	if err != nil {
		return fmt.Errorf("save release file: %w", err)
	}
	return nil
}

// ReleaseFiles lists copied files of repo ("" for every repo), by repo,
// version and name.
func (s *Store) ReleaseFiles(ctx context.Context, providerName, repo string) ([]ReleaseFile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, repo, tag, name, platform, size, sha256, fork, fork_release_id, fork_asset_id, status, error, updated_at
		FROM release_files WHERE (? = '' OR provider = ?) AND (? = '' OR repo = ?)`,
		providerName, providerName, repo, repo)
	if err != nil {
		return nil, fmt.Errorf("list release files: %w", err)
	}
	defer rows.Close()
	var out []ReleaseFile
	for rows.Next() {
		var f ReleaseFile
		var updated int64
		if err := rows.Scan(&f.Provider, &f.Repo, &f.Tag, &f.Name, &f.Platform, &f.Size, &f.SHA256, &f.Fork,
			&f.ForkReleaseID, &f.ForkAssetID, &f.Status, &f.Error, &updated); err != nil {
			return nil, err
		}
		f.UpdatedAt = time.UnixMilli(updated)
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b ReleaseFile) int {
		if c := strings.Compare(a.Repo, b.Repo); c != 0 {
			return c
		}
		if c := provider.CompareVersions(a.Tag, b.Tag); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, rows.Err()
}
