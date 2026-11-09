//go:build !darwin && !linux

package safefs

import (
	"errors"
	"os"
)

var ErrLimit = errors.New("safe filesystem limit exceeded")
var errUnsupported = errors.New("descriptor-relative filesystem requires Linux or macOS")

type Root struct{}
type Entry struct {
	Name string
	Size int64
	Mode uint32
}

func (Entry) IsDir() bool                     { return false }
func (Entry) IsRegular() bool                 { return false }
func Open(string) (*Root, error)              { return nil, errUnsupported }
func (*Root) Close() error                    { return nil }
func (*Root) MkdirAll(string) error           { return errUnsupported }
func (*Root) ChownDir(string, int, int) error { return errUnsupported }
func (*Root) OpenRegular(string, int64) (*os.File, os.FileInfo, error) {
	return nil, nil, errUnsupported
}
func (*Root) ReadFile(string, int64) ([]byte, error)      { return nil, errUnsupported }
func (*Root) WriteFile(string, []byte, os.FileMode) error { return errUnsupported }
func (*Root) Entries(string, int) ([]Entry, error)        { return nil, errUnsupported }
func (*Root) RemoveAll(string) error                      { return errUnsupported }
func (*Root) Bytes(int, int) (int64, error)               { return 0, errUnsupported }
