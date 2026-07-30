// Package codexlauncher selects fresh or resumed Codex startup from durable session state.
package codexlauncher

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func Command(codexBinary, codexHome string, arguments []string) ([]string, error) {
	if !filepath.IsAbs(codexBinary) || !filepath.IsAbs(codexHome) {
		return nil, fmt.Errorf("absolute Codex binary and home paths are required")
	}
	resume, err := hasSession(filepath.Join(codexHome, "sessions"))
	if err != nil {
		return nil, err
	}
	command := make([]string, 0, len(arguments)+3)
	command = append(command, codexBinary)
	command = append(command, arguments...)
	if resume {
		command = append(command, "resume", "--last")
	}
	return command, nil
}

func hasSession(root string) (bool, error) {
	found := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return fs.SkipDir
			}
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Size() > 0 {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}
