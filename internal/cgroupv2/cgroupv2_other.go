//go:build !linux

// Package cgroupv2 is a Linux facility; elsewhere NewPool reports that it is
// unavailable and callers fall back to what their platform offers.
package cgroupv2

import "errors"

var errUnsupported = errors.New("cgroups are a Linux facility")

// Options name a pool and the controllers its leaves need.
type Options struct {
	Unit        string
	LeafPrefix  string
	Controllers []string
}

// Setting is one interface file of a leaf and the value it must hold.
type Setting struct {
	File  string
	Value string
}

// Pool is never created outside Linux.
type Pool struct{}

// Leaf is never created outside Linux.
type Leaf struct{}

func NewPool(Options) (*Pool, error)             { return nil, errUnsupported }
func (p *Pool) NewLeaf([]Setting) (*Leaf, error) { return nil, errUnsupported }
func (p *Pool) Live() []string                   { return nil }
func (p *Pool) Close() error                     { return nil }
func (l *Leaf) Dir() string                      { return "" }
func (l *Leaf) Admit(int) error                  { return errUnsupported }
func (l *Leaf) Kill() error                      { return nil }
func (l *Leaf) Populated() bool                  { return false }
func (l *Leaf) Procs() []int                     { return nil }
func (l *Leaf) Read(string) (string, error)      { return "", errUnsupported }
func (l *Leaf) Remove() error                    { return nil }
func (l *Leaf) RemoveIfEmpty() bool              { return true }
