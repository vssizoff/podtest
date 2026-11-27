package problem

import (
	"archive/zip"
	"bytes"
	"context"
	"io/fs"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"podtest/internal/db"
)

const MaxPackageText = 256 << 10

func (m Manager) DraftPackage(ctx context.Context, problem, rev, name string) ([]string, *string, error) {
	var source *string
	var status, format string
	err := m.Pool.QueryRow(ctx, `SELECT source_artifact_id,status,execution_format FROM problem_revisions WHERE id=$1 AND problem_id=$2`, rev, problem).Scan(&source, &status, &format)
	if err != nil {
		return nil, nil, err
	}
	if status != "draft" {
		return nil, nil, ErrNotDraft
	}
	files, err := m.editablePackage(ctx, format, source)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	if name == "" {
		return names, nil, nil
	}
	data, ok := files[name]
	if !ok {
		return nil, nil, pgx.ErrNoRows
	}
	if len(data) > MaxPackageText || !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		return nil, nil, ErrInvalid
	}
	text := string(data)
	return names, &text, nil
}

func (m Manager) editablePackage(ctx context.Context, format string, source *string) (map[string][]byte, error) {
	if format != "standard" && format != "scripted_lua" {
		return nil, ErrUnsupported
	}
	if source == nil {
		return map[string][]byte{}, nil
	}
	data, err := m.Artifacts.Read(ctx, *source, MaxPackageBytes)
	if err != nil {
		return nil, err
	}
	return Files(data)
}

// EditPackageFile permits incomplete drafts. Validate and Publish enforce the execution contract.
func (m Manager) EditPackageFile(ctx context.Context, actor, problem, rev, name string, data []byte, remove bool) error {
	if len(data) > MaxPackageText || !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		return ErrInvalid
	}
	if !fs.ValidPath(name) || name == "." || len(name) > 255 || strings.ContainsAny(name, "\\:\x00") {
		return ErrArchive
	}
	if name == "podtest-manifest.json" {
		return ErrReservedFile
	}
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if err := draft(ctx, tx, problem, rev); err != nil {
			return err
		}
		var source *string
		var format string
		if err := tx.QueryRow(ctx, `SELECT source_artifact_id,execution_format FROM problem_revisions WHERE id=$1`, rev).Scan(&source, &format); err != nil {
			return err
		}
		files, err := m.editablePackage(ctx, format, source)
		if err != nil {
			return err
		}
		if remove {
			if _, ok := files[name]; !ok {
				return nil
			}
			delete(files, name)
		} else {
			files[name] = data
		}
		archive, err := encodeDraftPackage(files)
		if err != nil {
			return err
		}
		ref, err := m.Artifacts.PutTx(ctx, tx, actor, "problem-import", "application/zip", archive)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE problem_revisions SET source_artifact_id=$2 WHERE id=$1`, rev, ref.ID)
		return err
	})
}

func encodeDraftPackage(files map[string][]byte) ([]byte, error) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err = entry.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if archive.Len() > MaxPackageBytes {
		return nil, ErrArchive
	}
	if _, err := Files(archive.Bytes()); err != nil {
		return nil, err
	}
	return archive.Bytes(), nil
}
