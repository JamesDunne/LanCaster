// tarball
package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type VirtualTarballWriter struct {
	files tarballFileList
	size  int64

	// Which file is currently open for writing:
	openFileInfo *TarballFile
	openFile     *os.File
}

func NewVirtualTarballWriter(files []*TarballFile) (*VirtualTarballWriter, error) {
	filesInternal := tarballFileList(make([]*TarballFile, 0, len(files)))

	uniquePaths := make(map[string]string)
	size := int64(0)
	for _, f := range files {
		// Validate paths:
		if filepath.IsAbs(f.Path) {
			return nil, ErrBadPAth
		}
		s := strings.Split(f.Path, string(filepath.Separator))
		for _, p := range s {
			if p == "." || p == ".." {
				return nil, ErrBadPAth
			}
		}

		// Validate all paths are unique:
		if _, ok := uniquePaths[f.Path]; ok {
			return nil, ErrDuplicatePaths
		}
		uniquePaths[f.Path] = f.Path

		f.offset = size
		filesInternal = append(filesInternal, f)

		// Each file ends with a terminating NUL character so at least one call to WriteAt or ReadAt will happen to create/read all files.
		size += f.Size + 1
	}

	// Sort files for consistency:
	sort.Sort(filesInternal)

	return &VirtualTarballWriter{
		files: filesInternal,
		size:  size,
	}, nil
}

func (t *VirtualTarballWriter) closeFile() error {
	if t.openFileInfo == nil {
		t.openFile = nil
		return nil
	}
	if t.openFile == nil {
		t.openFileInfo = nil
		return nil
	}

	err := t.openFile.Chmod(t.openFileInfo.Mode)
	if err != nil {
		return err
	}

	err = t.openFile.Close()
	if err != nil {
		return err
	}

	t.openFile = nil
	t.openFileInfo = nil
	return nil
}

// io.Closer:
func (t *VirtualTarballWriter) Close() error {
	return t.closeFile()
}

func (t *VirtualTarballWriter) makeSymlink(tf *TarballFile) error {
	_, err := os.Lstat(tf.Path)
	if err != nil {
		// Dont bother recreating if exists:
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Get current working directory:
	wd := ""
	wd, err = os.Getwd()
	if err != nil {
		return err
	}

	err = os.Chdir(filepath.Base(tf.Path))
	if err != nil {
		return err
	}

	// Change directory back to what it was before exiting:
	defer func() {
		err = os.Chdir(wd)
	}()

	// Create symlink from directory:
	err = os.Symlink(tf.Path, tf.SymlinkDestination)

	// Return the last error (possibly from defer):
	return err
}

// io.WriterAt:
func (t *VirtualTarballWriter) WriteAt(buf []byte, offset int64) (int, error) {
	if buf == nil {
		return 0, ErrNilBuffer
	}
	if offset < 0 || offset >= t.size {
		return 0, ErrOutOfRange
	}

	// Write to file(s):
	total := 0
	remainder := buf[:]
	for _, tf := range t.files {
		if offset < tf.offset || offset >= tf.offset+tf.Size+1 {
			continue
		}

		if tf.Mode&os.ModeSymlink == os.ModeSymlink {
			// Create symlink if not exists:
			err := t.makeSymlink(tf)
			if err != nil {
				return 0, err
			}
		} else if tf.Size > 0 {
			// Create file if not already:
			if t.openFileInfo != tf {
				// Close and finalize last open file:
				if t.openFileInfo != nil {
					t.closeFile()
				}

				// Try to mkdir all paths involved:
				dir, _ := filepath.Split(tf.Path)
				if dir != "" {
					// TODO: record directory entries for their modes.
					// Make sure directories are at least rwx by owner:
					err := os.MkdirAll(dir, tf.Mode|0700)
					if err != nil {
						return 0, err
					}
				}

				f, err := os.OpenFile(tf.Path, os.O_WRONLY|os.O_CREATE, tf.Mode|0700)
				if err != nil {
					if os.IsPermission(err) {
						// chmod existing file to be able to write:
						err = os.Chmod(tf.Path, tf.Mode|0700)
						if err != nil {
							return 0, err
						}
						// Try to reopen for writing:
						f, err = os.OpenFile(tf.Path, os.O_WRONLY|os.O_CREATE, tf.Mode|0700)
					}
					if err != nil {
						return 0, err
					}
				}

				// Reserve disk space:
				err = f.Truncate(tf.Size)
				if err != nil {
					return 0, err
				}

				t.openFile = f
				t.openFileInfo = tf
			}
		}

		localOffset := offset - tf.offset
		if localOffset < tf.Size {
			// Perform write:
			p := remainder
			if localOffset+int64(len(p)) > tf.Size {
				p = remainder[:tf.Size-localOffset]
			}
			if len(p) > 0 {
				// NOTE: we allow len(p) == 0 to create file as a side effect in case that's useful.
				n, err := t.openFile.WriteAt(p, localOffset)
				if err != nil {
					return 0, err
				}
				total += n
				offset += int64(n)
				localOffset += int64(n)
				remainder = remainder[n:]
			}
		}

		// Expect trailing NUL padding byte:
		if offset == tf.offset+tf.Size && len(remainder) > 0 {
			if remainder[0] != 0 {
				return 0, ErrBadPaddingByte
			}
			remainder = remainder[1:]
			offset++
			total++
		}

		// Keep iterating files until we have no more to write:
		if len(remainder) == 0 {
			break
		}
	}

	return total, nil
}
