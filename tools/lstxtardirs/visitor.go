package lstxtardirs

import (
	"io/fs"
	"os"
	"path/filepath"
)

type TxtarDirVisitor struct {
	rootDir string
	cb      func(path string) error
}

func (d *TxtarDirVisitor) Walk() error {
	return filepath.WalkDir(d.rootDir, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !de.IsDir() {
			return nil
		}

		matches, err := fs.Glob(os.DirFS(path), "*txtar")
		if err != nil {
			return err
		}

		if len(matches) > 0 {
			return d.cb(path)
		}

		return nil
	})
}

func NewVisitor(rootDir string, cb func(path string) error) *TxtarDirVisitor {
	return &TxtarDirVisitor{
		rootDir: rootDir,
		cb:      cb,
	}
}
