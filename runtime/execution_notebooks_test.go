package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/ipynb"
)

// executionFake is the deterministic execution runtime an execution-notebook test runs against. It
// keeps the kernel list the notebook reads, records every request, and answers by code text, so the
// notebook behaviour is checked without starting a worker or a model request.
type executionFake struct {
	mu        sync.Mutex
	manifests []evalapi.Manifest
	kernels   []evalapi.Session
	created   []string
	executed  []evalapi.Request
	controls  []string
	answers   map[string]evalapi.Result
	artifacts map[string][]byte
	failWith  error
	nextID    int
	// answerByKernel answers a request from the state of one kernel, for the tests that model a cell
	// whose result depends on what the kernel already holds. nil answers from answers instead.
	answerByKernel func(kernelID, code string) (evalapi.Result, bool)
	// block, when set, is waited on before a request is answered: a test that needs a run to be in
	// flight observes the run registry while it is held.
	block chan struct{}
}

func newExecutionFake() *executionFake {
	return &executionFake{
		manifests: []evalapi.Manifest{
			{ID: "javascript", Language: "javascript", Version: "1.0", RuntimeVersion: "22"},
			{ID: "python", Language: "python", Version: "1.0", RuntimeVersion: "3.12"},
		},
		answers:   map[string]evalapi.Result{},
		artifacts: map[string][]byte{},
	}
}

func (f *executionFake) runtimes(context.Context) ([]evalapi.Manifest, error) {
	return f.manifests, nil
}

func (f *executionFake) sessions(context.Context, string) ([]evalapi.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]evalapi.Session(nil), f.kernels...), nil
}

func (f *executionFake) create(_ context.Context, _ string, runtimeID string) (evalapi.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	language := runtimeID
	for _, manifest := range f.manifests {
		if manifest.ID == runtimeID {
			language = manifest.Language
		}
	}
	kernel := evalapi.Session{
		ID: fmt.Sprintf("kernel-%d", f.nextID), RuntimeID: runtimeID, Language: language,
		State: evalapi.StateReady, Generation: uint64(f.nextID),
	}
	f.kernels = append(f.kernels, kernel)
	f.created = append(f.created, kernel.ID)
	return kernel, nil
}

func (f *executionFake) execute(_ context.Context, _ string, req evalapi.Request) (evalapi.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executed = append(f.executed, req)
	if f.block != nil {
		block := f.block
		f.mu.Unlock()
		<-block
		f.mu.Lock()
	}
	if f.failWith != nil {
		return evalapi.Result{}, f.failWith
	}
	var result evalapi.Result
	ok := false
	if f.answerByKernel != nil {
		result, ok = f.answerByKernel(req.SessionID, req.Code)
	}
	if !ok {
		result, ok = f.answers[req.Code]
	}
	if !ok {
		result = evalapi.Result{Status: evalapi.ResultStatusCompleted, Stdout: "ran " + req.Code + "\n", DurationMS: 7}
	}
	result.RequestID = fmt.Sprintf("req-%d", len(f.executed))
	result.SessionID = req.SessionID
	result.CellID = req.CellID
	if result.Generation == 0 {
		for _, kernel := range f.kernels {
			if kernel.ID == req.SessionID {
				result.Generation = kernel.Generation
			}
		}
	}
	return result, nil
}

func (f *executionFake) control(_ context.Context, _ string, evalSessionID, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controls = append(f.controls, action+":"+evalSessionID)
	for i := range f.kernels {
		if f.kernels[i].ID != evalSessionID {
			continue
		}
		switch action {
		case "close":
			f.kernels[i].State = evalapi.StateStopped
		case "reset":
			f.kernels[i].Generation++
			f.kernels[i].State = evalapi.StateReady
		}
	}
	return nil
}

func (f *executionFake) artifact(_ context.Context, _ string, evalSessionID, artifactID string) (evalapi.Artifact, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.artifacts[artifactID]
	if !ok {
		return evalapi.Artifact{}, nil, fmt.Errorf("no artifact %s", artifactID)
	}
	return evalapi.Artifact{ID: artifactID, SessionID: evalSessionID, MIME: "image/png", Size: int64(len(data))}, data, nil
}

// executionNotebookHostForTest binds the execution-notebook host to a real Engine and a fake runtime.
func executionNotebookHostForTest(t *testing.T) (*Engine, string, string, *executionFake, *executionNotebookHost) {
	t.Helper()
	workspace := t.TempDir()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "execution notebook"})
	if err != nil {
		t.Fatal(err)
	}
	fake := newExecutionFake()
	return engine, handle.ID, workspace, fake, &executionNotebookHost{engine: engine, exec: fake}
}

func TestExecutionNotebookCRUD(t *testing.T) {
	engine, sessionID, workspace, _, host := executionNotebookHostForTest(t)
	ctx := context.Background()

	created, err := host.create(ctx, sessionID, "Analysis", "python")
	if err != nil {
		t.Fatal(err)
	}
	if created.Title != "Analysis" || created.Version != 1 || created.Document.DefaultLanguage() != "python" {
		t.Fatalf("created = %#v", created)
	}
	if created.Document.NBFormat != ipynb.Format || created.Document.NBFormatMinor != ipynb.FormatMinor {
		t.Fatalf("format = %d.%d", created.Document.NBFormat, created.Document.NBFormatMinor)
	}

	// An agent working notebook is a different kind and stays out of this API.
	if _, err := engine.CreateNotebook(ctx, sessionID, "plan", "# Plan\n"); err != nil {
		t.Fatal(err)
	}
	list, err := host.list(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "Analysis" {
		t.Fatalf("list = %#v", list)
	}
	if _, err := host.get(ctx, sessionID, "plan"); !errors.Is(err, ErrExecutionNotebookNotFound) {
		t.Fatalf("reading a working notebook = %v", err)
	}
	if _, err := host.save(ctx, sessionID, "plan", ipynb.New("python"), 1); !errors.Is(err, ErrExecutionNotebookNotFound) {
		t.Fatalf("saving a working notebook = %v", err)
	}

	// A cell edit applies to the version it names and bumps the version by one.
	added, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "print(1)"}, -1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if added.Version != 2 || len(added.Document.Cells) != 1 {
		t.Fatalf("added = %#v", added)
	}
	cell := added.Document.Cells[0]
	if cell.ID == "" || cell.Metadata.LC == nil || cell.Metadata.LC.Language != "python" {
		t.Fatalf("cell = %#v", cell)
	}
	if _, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellMarkdown, Source: "note"}, -1, 1); !errors.Is(err, ErrNotebookConflict) {
		t.Fatalf("a stale version wrote anyway: %v", err)
	}

	// Markdown cells carry no outputs even when a caller sends some.
	markdown, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{
		CellType: ipynb.CellMarkdown, Source: "note",
		Outputs: []ipynb.Output{{OutputType: ipynb.OutputStream, Name: ipynb.StreamStdout, Text: "stray"}},
	}, -1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(markdown.Document.Cells[1].Outputs) != 0 {
		t.Fatal("a markdown cell kept an output")
	}

	moved, err := host.moveCell(ctx, sessionID, created.ID, cell.ID, 1, markdown.Version)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Document.Cells[1].ID != cell.ID {
		t.Fatalf("move order = %#v", moved.Document.Cells)
	}

	replaced := moved.Document.Cells[1]
	replaced.Source = "print(2)"
	updated, err := host.updateCell(ctx, sessionID, created.ID, replaced, moved.Version)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated.Document.Cells[1].Source) != "print(2)" {
		t.Fatal("update did not write the cell")
	}

	deleted, err := host.deleteCell(ctx, sessionID, created.ID, cell.ID, updated.Version)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted.Document.Cells) != 1 || deleted.Document.Cells[0].CellType != ipynb.CellMarkdown {
		t.Fatalf("after delete = %#v", deleted.Document.Cells)
	}

	document := deleted.Document
	document.SetDefaultLanguage("javascript")
	saved, err := host.save(ctx, sessionID, created.ID, document, deleted.Version)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Document.DefaultLanguage() != "javascript" || saved.Version != deleted.Version+1 {
		t.Fatalf("saved = %#v", saved)
	}

	// The document is durable: a fresh Engine reads the same version and document.
	reopened, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.ExecutionNotebook(ctx, sessionID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != saved.Version || again.Document.DefaultLanguage() != "javascript" || len(again.Document.Cells) != 1 {
		t.Fatalf("after reopen = %#v", again)
	}

	if err := host.delete(ctx, sessionID, created.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := host.list(ctx, sessionID); err != nil || len(list) != 0 {
		t.Fatalf("after delete: %#v, %v", list, err)
	}
}

func TestExecutionNotebookImportAndExport(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()

	imported, err := host.importNotebook(ctx, sessionID, "Imported", standardImport)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Document.DefaultLanguage() != "python" || len(imported.Document.Cells) != 2 {
		t.Fatalf("imported = %#v", imported)
	}
	if len(fake.executed) != 0 || len(fake.created) != 0 {
		t.Fatal("importing a notebook executed something")
	}
	if got := imported.Document.CellLanguage(imported.Document.Cells[1]); got != "python" {
		t.Fatalf("imported cell language = %q", got)
	}
	// Recorded outputs are evidence of the run that produced them; they are kept, not replayed.
	if len(imported.Document.Cells[1].Outputs) == 0 {
		t.Fatal("import dropped the recorded outputs")
	}

	document, filename, err := host.export(ctx, sessionID, imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if filename != "Imported.ipynb" {
		t.Fatalf("filename = %q", filename)
	}
	reparsed, err := ipynb.Parse(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(reparsed.Cells) != 2 || reparsed.DefaultLanguage() != "python" {
		t.Fatalf("exported = %#v", reparsed)
	}

	if _, err := host.importNotebook(ctx, sessionID, "Broken", `{"nbformat": 3}`); err == nil {
		t.Fatal("an unsupported notebook was imported")
	}
	if _, err := host.importNotebook(ctx, sessionID, "Imported", standardImport); err == nil {
		t.Fatal("a second notebook with the same title was accepted")
	}
}

// standardImport is a small nbformat 4.5 notebook with a recorded output.
const standardImport = `{
 "nbformat": 4,
 "nbformat_minor": 5,
 "metadata": {"kernelspec": {"name": "python3", "language": "python"}},
 "cells": [
  {"cell_type": "markdown", "metadata": {}, "source": "Read the data."},
  {"cell_type": "code", "metadata": {}, "execution_count": 1, "source": "print(1)",
   "outputs": [{"output_type": "stream", "name": "stdout", "text": "1\n"}]}
 ]
}`

func TestExecutionNotebookRunRecordsOutputsAndHistory(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()

	created, err := host.create(ctx, sessionID, "Run", "python")
	if err != nil {
		t.Fatal(err)
	}
	first, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "print(1)"}, -1, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "print(2)"}, -1, first.Version)
	if err != nil {
		t.Fatal(err)
	}

	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("kernels created = %v", fake.created)
	}
	if len(fake.executed) != 2 {
		t.Fatalf("executed = %d", len(fake.executed))
	}
	if fake.executed[0].SessionID != fake.executed[1].SessionID {
		t.Fatal("the two cells of one language ran on different kernels")
	}
	for _, cell := range ran.Document.Cells {
		if cell.Metadata.LC == nil || cell.Metadata.LC.Status != ipynb.StatusOK {
			t.Fatalf("cell status = %#v", cell.Metadata.LC)
		}
		if cell.ExecutionCount == nil || *cell.ExecutionCount == 0 {
			t.Fatalf("cell execution count = %v", cell.ExecutionCount)
		}
		if len(cell.Outputs) == 0 || !strings.Contains(string(cell.Outputs[0].Text), "ran ") {
			t.Fatalf("cell outputs = %#v", cell.Outputs)
		}
		if cell.Metadata.LC.Kernel == "" || cell.Metadata.LC.RequestID == "" {
			t.Fatalf("cell kernel provenance = %#v", cell.Metadata.LC)
		}
	}
	history := ran.Document.Extension().History
	if len(history) != 2 {
		t.Fatalf("history = %#v", history)
	}
	if history[0].CellID != first.Document.Cells[0].ID || history[1].CellID != second.Document.Cells[1].ID {
		t.Fatalf("history order = %q, %q", history[0].CellID, history[1].CellID)
	}
	if ran.Document.Extension().ExecutionCount != 2 {
		t.Fatalf("notebook execution count = %d", ran.Document.Extension().ExecutionCount)
	}

	// A second run reuses the live kernel instead of starting another one, and the notebook counter
	// advances by one per executed cell.
	second, err = host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("a live kernel was not reused: %v", fake.created)
	}
	if second.Document.Extension().ExecutionCount != ran.Document.Extension().ExecutionCount+2 {
		t.Fatalf("a two-cell run advanced the counter %d -> %d, want +2",
			ran.Document.Extension().ExecutionCount, second.Document.Extension().ExecutionCount)
	}

	// One cell only: exactly one more execution and one more count.
	before := len(fake.executed)
	one, err := host.run(ctx, sessionID, created.ID, ran.Document.Cells[0].ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.executed) != before+1 {
		t.Fatalf("a single-cell run executed %d cells", len(fake.executed)-before)
	}
	if one.Document.Extension().ExecutionCount != second.Document.Extension().ExecutionCount+1 {
		t.Fatalf("execution count after a single cell = %d, want %d",
			one.Document.Extension().ExecutionCount, second.Document.Extension().ExecutionCount+1)
	}
	if _, err := host.run(ctx, sessionID, created.ID, "missing", false); err == nil {
		t.Fatal("running an unknown cell was accepted")
	}
}

func TestExecutionNotebookStopsAtFailureAndReplaysClean(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()

	created, err := host.create(ctx, sessionID, "Replay", "python")
	if err != nil {
		t.Fatal(err)
	}
	doc := created.Document
	for _, source := range []string{"ok-1", "boom", "never"} {
		if err := doc.Insert(-1, ipynb.Cell{CellType: ipynb.CellCode, Source: ipynb.Source(source)}); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := host.save(ctx, sessionID, created.ID, doc, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	fake.answers["boom"] = evalapi.Result{Status: evalapi.ResultStatusFailed, Error: "ValueError: bad", Traceback: "line1\nline2", ExitCode: 1}

	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	statuses := []string{}
	for _, cell := range ran.Document.Cells {
		statuses = append(statuses, cellStatus(ran.Document, cell.ID))
	}
	if statuses[0] != ipynb.StatusOK || statuses[1] != ipynb.StatusError {
		t.Fatalf("statuses = %v", statuses)
	}
	if statuses[2] == ipynb.StatusOK {
		t.Fatalf("the cell after the failure claims success: %v", statuses)
	}
	if len(fake.executed) != 2 {
		t.Fatalf("a failing cell did not stop the run: %d executed", len(fake.executed))
	}
	failed := ran.Document.Cells[1]
	if len(failed.Outputs) == 0 || failed.Outputs[len(failed.Outputs)-1].OutputType != ipynb.OutputError {
		t.Fatalf("failed cell outputs = %#v", failed.Outputs)
	}
	if failed.Metadata.LC.ExitCode == nil || *failed.Metadata.LC.ExitCode != 1 {
		t.Fatalf("exit code = %v", failed.Metadata.LC.ExitCode)
	}
	if history := ran.Document.Extension().History; len(history) != 2 || history[1].Status != ipynb.StatusError {
		t.Fatalf("history = %#v", history)
	}

	// Replay closes the kernels, clears every recorded result and runs the notebook again.
	before := len(fake.executed)
	replayed, err := host.run(ctx, sessionID, created.ID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.controls) != 1 || !strings.HasPrefix(fake.controls[0], "close:") {
		t.Fatalf("replay controls = %v", fake.controls)
	}
	if len(fake.created) != 2 {
		t.Fatalf("replay did not start a clean kernel: %v", fake.created)
	}
	if len(fake.executed)-before != 2 {
		t.Fatalf("replay executed %d cells", len(fake.executed)-before)
	}
	if replayed.Document.Extension().ExecutionCount != 2 {
		t.Fatalf("replay kept the old counter: %d", replayed.Document.Extension().ExecutionCount)
	}
	if got := replayed.Document.Cells[2].Metadata.LC.Status; got != ipynb.StatusIdle {
		t.Fatalf("the cell after the failure = %q", got)
	}
	if saved.Version == replayed.Version {
		t.Fatal("replay wrote nothing")
	}
}

func TestExecutionNotebookKernelControlAndArtifacts(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()

	created, err := host.create(ctx, sessionID, "Kernels", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "chart()"}, -1, created.Version); err != nil {
		t.Fatal(err)
	}
	fake.answers["chart()"] = evalapi.Result{
		Status: evalapi.ResultStatusCompleted, Stdout: "drew\n",
		Artifacts: []evalapi.Artifact{{ID: "art-1", SessionID: "kernel-1", CellID: "cell", MIME: "image/png", Size: 3}},
	}
	fake.artifacts["art-1"] = []byte("png")

	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	cell := ran.Document.Cells[0]
	var reference *ipynb.OutputLC
	for _, output := range cell.Outputs {
		if output.LC != nil && output.LC.MIME == "image/png" {
			reference = output.LC
		}
		if len(output.Data) > 0 {
			t.Fatalf("the artifact's bytes were written into the document: %#v", output.Data)
		}
	}
	if reference == nil || reference.NotDurable || reference.ArtifactID == "" {
		t.Fatalf("a small artifact was not kept durably: %#v", reference)
	}
	stored, err := host.notebookArtifact(ctx, sessionID, reference.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Data) != "png" || stored.MIME != "image/png" {
		t.Fatalf("stored artifact = %#v", stored)
	}
	if history := ran.Document.Extension().History; len(history) != 1 || len(history[0].Artifacts) != 1 || history[0].Artifacts[0] != "art-1" {
		t.Fatalf("history artifacts = %#v", history)
	}

	artifact, err := host.artifact(ctx, sessionID, "kernel-1", "art-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact.Data) != "png" || artifact.MIME != "image/png" {
		t.Fatalf("artifact = %#v", artifact)
	}
	if _, err := host.artifact(ctx, sessionID, "kernel-1", "missing"); err == nil {
		t.Fatal("an unknown artifact was returned")
	}

	kernels, err := host.kernels(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kernels) != 1 || !kernels[0].Alive || kernels[0].Language != "python" {
		t.Fatalf("kernels = %#v", kernels)
	}
	if err := host.controlLanguage(ctx, sessionID, "python", "reset"); err != nil {
		t.Fatal(err)
	}
	if len(fake.controls) != 1 || !strings.HasPrefix(fake.controls[0], "reset:") {
		t.Fatalf("controls = %v", fake.controls)
	}
	if err := host.interrupt(ctx, sessionID, created.ID); err != nil {
		t.Fatal(err)
	}
	if len(fake.controls) != 2 || !strings.HasPrefix(fake.controls[1], "interrupt:") {
		t.Fatalf("controls = %v", fake.controls)
	}
	if err := host.controlLanguage(ctx, sessionID, "python", "close"); err != nil {
		t.Fatal(err)
	}
	if err := host.controlLanguage(ctx, sessionID, "python", "close"); err != nil {
		t.Fatal(err)
	}
	if len(fake.controls) != 3 {
		t.Fatalf("closing a stopped kernel called the runtime again: %v", fake.controls)
	}
	// A language with no kernel is a no-op, not an error.
	if err := host.controlLanguage(ctx, sessionID, "javascript", "close"); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionNotebookRecordsInfrastructureFailure(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()

	created, err := host.create(ctx, sessionID, "Broken", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "x"}, -1, created.Version); err != nil {
		t.Fatal(err)
	}
	fake.failWith = errors.New("worker exited")
	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err == nil {
		t.Fatal("an execution failure was reported as success")
	}
	if ran.ID == "" {
		t.Fatal("the notebook was not returned with the failure")
	}
	cell := ran.Document.Cells[0]
	if cell.Metadata.LC.Status != ipynb.StatusError || len(cell.Outputs) == 0 {
		t.Fatalf("cell = %#v", cell)
	}
	if !strings.Contains(cell.Outputs[0].EValue, "worker exited") {
		t.Fatalf("failure output = %#v", cell.Outputs[0])
	}
}

func TestExecutionNotebookLanguageAndTitleValidation(t *testing.T) {
	_, sessionID, _, _, host := executionNotebookHostForTest(t)
	ctx := context.Background()

	if _, err := host.create(ctx, sessionID, "  ", "python"); err == nil {
		t.Fatal("an empty title was accepted")
	}
	if _, err := host.create(ctx, sessionID, "Cobol", "cobol"); err == nil {
		t.Fatal("a language no runtime provides was accepted")
	}
	created, err := host.create(ctx, sessionID, "Mixed", "")
	if err != nil {
		t.Fatal(err)
	}
	if created.Document.DefaultLanguage() != "" {
		t.Fatalf("default language = %q", created.Document.DefaultLanguage())
	}
	// A cell without a language is recorded as an error and stops the run.
	added, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "1"}, -1, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if ran.Version != added.Version+1 {
		t.Fatalf("version = %d, want %d", ran.Version, added.Version+1)
	}
	if got := ran.Document.Cells[0].Metadata.LC.Status; got != ipynb.StatusError {
		t.Fatalf("status = %q", got)
	}
}

func TestExecutionNotebookOutputMapping(t *testing.T) {
	result := evalapi.Result{
		Status: evalapi.ResultStatusCompleted, Stdout: "out\n", Stderr: "warn\n", Error: "", ExitCode: 0,
		Displays: []evalapi.Display{{MIME: "text/plain", Encoding: "utf8", Text: "42"}, {MIME: "image/png", Encoding: "base64", Text: "cG5n"}},
	}
	outputs := resultOutputs(result, "kernel", nil)
	if len(outputs) != 4 {
		t.Fatalf("outputs = %#v", outputs)
	}
	if outputs[0].Name != ipynb.StreamStdout || string(outputs[0].Text) != "out\n" {
		t.Fatalf("stdout = %#v", outputs[0])
	}
	if outputs[1].Name != ipynb.StreamStderr {
		t.Fatalf("stderr = %#v", outputs[1])
	}
	if got := outputs[2].Data["text/plain"].String(); got != "42" {
		t.Fatalf("display = %#v", outputs[2].Data)
	}
	if outputs[2].LC != nil {
		t.Fatalf("a plain display carries an extension: %#v", outputs[2].LC)
	}

	// A display that names an artifact is one output: the stored reference, and the inline payload
	// only when it fits. An artifact the document does not hold is marked volatile, never durable.
	refs := map[string]ipynb.OutputLC{"sha-1": {ArtifactID: "sha-1", SessionID: "s1", MIME: "image/png", Size: 3}}
	referenced := displayOutputs([]evalapi.Display{{MIME: "image/png", ArtifactID: "sha-1", Text: "cG5n"}}, "kernel", refs)
	if len(referenced) != 1 {
		t.Fatalf("referenced display = %#v", referenced)
	}
	if referenced[0].LC == nil || referenced[0].LC.ArtifactID != "sha-1" || referenced[0].LC.SessionID != "s1" || referenced[0].LC.NotDurable {
		t.Fatalf("referenced display = %#v", referenced[0].LC)
	}
	if got := referenced[0].Data["image/png"].String(); got != "cG5n" {
		t.Fatalf("a small inline payload was dropped: %#v", referenced[0].Data)
	}
	volatile := displayOutputs([]evalapi.Display{{MIME: "image/png", ArtifactID: "runtime-1", Size: 9}}, "kernel", nil)
	if len(volatile) != 1 || volatile[0].LC == nil || !volatile[0].LC.NotDurable || volatile[0].LC.SessionID != "kernel" {
		t.Fatalf("volatile display = %#v", volatile)
	}
	if len(volatile[0].Data) != 0 {
		t.Fatalf("a reference-only display inlined bytes: %#v", volatile[0].Data)
	}

	failed := resultOutputs(evalapi.Result{Status: evalapi.ResultStatusFailed, Error: "boom", Traceback: "a\nb", ExitCode: 2}, "k", nil)
	last := failed[len(failed)-1]
	if last.OutputType != ipynb.OutputError || last.EValue != "boom" || len(last.Traceback) != 2 {
		t.Fatalf("error output = %#v", last)
	}
	if resultStatus(evalapi.Result{Status: evalapi.ResultStatusCancelled}) != ipynb.StatusCancelled {
		t.Fatal("a cancelled result did not map to cancelled")
	}
	// An unknown status is never a pass: a result the runtime did not classify cannot claim success.
	if got := resultStatus(evalapi.Result{}); got != ipynb.StatusError {
		t.Fatalf("an unknown result status mapped to %q", got)
	}
	if got := resultStatus(evalapi.Result{Status: "something-new", ExitCode: 0}); got != ipynb.StatusError {
		t.Fatalf("an unrecognised result status mapped to %q", got)
	}
	if resultStatus(evalapi.Result{ExitCode: 1}) != ipynb.StatusError {
		t.Fatal("a non-zero exit code did not map to error")
	}
}

// TestExecutionNotebookPublishesTerminalCellEvent: every way a cell's turn ends publishes how it
// ended. A page that showed the cell as running from the start event must not keep it running after
// the cell failed, or after a cell that could not start at all.
func TestExecutionNotebookPublishesTerminalCellEvent(t *testing.T) {
	engine, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	events, unsubscribe := engine.Events(256)
	defer unsubscribe()

	created, err := host.create(ctx, sessionID, "Failure", "python")
	if err != nil {
		t.Fatal(err)
	}
	added, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "boom()"}, -1, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	failedCell := added.Document.Cells[0].ID
	fake.failWith = errors.New("worker exited")
	if _, err := host.run(ctx, sessionID, created.ID, "", false); err == nil {
		t.Fatal("an execution failure was reported as success")
	}
	statuses := cellEventStatuses(t, events, failedCell)
	if len(statuses) < 2 || statuses[0] != ipynb.StatusRunning || statuses[len(statuses)-1] != ipynb.StatusError {
		t.Fatalf("events of a failed cell = %v", statuses)
	}

	// A cell with no language never reaches the kernel, and still ends.
	fake.failWith = nil
	blank, err := host.create(ctx, sessionID, "NoLanguage", "")
	if err != nil {
		t.Fatal(err)
	}
	blankAdded, err := host.addCell(ctx, sessionID, blank.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "1"}, -1, blank.Version)
	if err != nil {
		t.Fatal(err)
	}
	blankCell := blankAdded.Document.Cells[0].ID
	if _, err := host.run(ctx, sessionID, blank.ID, "", false); err != nil {
		t.Fatal(err)
	}
	statuses = cellEventStatuses(t, events, blankCell)
	if len(statuses) == 0 || statuses[len(statuses)-1] != ipynb.StatusError {
		t.Fatalf("events of a cell with no language = %v", statuses)
	}
}

// TestExecutionNotebookLargeDisplayIsOneArtifactOutput: a display past the inline limit that the
// runtime also stored as an artifact is one output — the durable reference — rather than a broken
// inline copy beside a duplicate artifact.
func TestExecutionNotebookLargeDisplayIsOneArtifactOutput(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	created, err := host.create(ctx, sessionID, "Chart", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "chart()"}, -1, created.Version); err != nil {
		t.Fatal(err)
	}
	pixels := bytes.Repeat([]byte("p"), ipynb.MaxInlineBytes+1)
	fake.answers["chart()"] = evalapi.Result{
		Status: evalapi.ResultStatusCompleted,
		Displays: []evalapi.Display{{
			MIME: "image/png", Encoding: "base64", Text: base64.StdEncoding.EncodeToString(pixels),
			ArtifactID: "art-1", Size: int64(len(pixels)),
		}},
		Artifacts: []evalapi.Artifact{{ID: "art-1", SessionID: "kernel-1", MIME: "image/png", Size: int64(len(pixels))}},
	}
	fake.artifacts["art-1"] = pixels

	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	outputs := ran.Document.Cells[0].Outputs
	if len(outputs) != 1 {
		t.Fatalf("a large display became %d outputs: %#v", len(outputs), outputs)
	}
	output := outputs[0]
	if output.LC == nil || output.LC.ArtifactID == "" || output.LC.SessionID != sessionID || output.LC.NotDurable {
		t.Fatalf("the display holds no durable reference: %#v", output.LC)
	}
	if len(output.Data) != 0 {
		t.Fatalf("the stored document inlined %d bytes", len(output.Data["image/png"].String()))
	}
	if output.LC.Truncated {
		t.Fatal("a display with a durable artifact was marked truncated")
	}
	stored, err := host.notebookArtifact(ctx, sessionID, output.LC.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.Data, pixels) || stored.MIME != "image/png" {
		t.Fatalf("the stored artifact is not the display's bytes: %d bytes, %q", len(stored.Data), stored.MIME)
	}
}

// cellEventStatuses drains the execution_notebook.cell events already published for one cell.
func cellEventStatuses(t *testing.T, events <-chan Event, cellID string) []string {
	t.Helper()
	var statuses []string
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return statuses
			}
			if event.Type != "execution_notebook.cell" {
				continue
			}
			var data struct {
				CellID string `json:"cellId"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatalf("cell event payload: %v", err)
			}
			if data.CellID == cellID {
				statuses = append(statuses, data.Status)
			}
		default:
			return statuses
		}
	}
}

// TestExecutionNotebookRunIsNotAutomatic: reading, listing and importing never execute a cell, which
// is what keeps opening a notebook from replaying someone else's work.
func TestExecutionNotebookRunIsNotAutomatic(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	created, err := host.importNotebook(ctx, sessionID, "Imported", standardImport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.get(ctx, sessionID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := host.list(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.export(ctx, sessionID, created.ID); err != nil {
		t.Fatal(err)
	}
	if len(fake.executed) != 0 || len(fake.created) != 0 {
		t.Fatalf("reading or importing executed something: %d requests, %d kernels", len(fake.executed), len(fake.created))
	}
}

// TestExecutionNotebookExportInlinesSmallArtifacts: an exported .ipynb opens outside LumenCortex, so a
// small artifact is written into the document and a large one keeps a readable note.
func TestExecutionNotebookExportInlinesSmallArtifacts(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	created, err := host.create(ctx, sessionID, "Chart", "python")
	if err != nil {
		t.Fatal(err)
	}
	added, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "chart()"}, -1, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	fake.answers["chart()"] = evalapi.Result{
		Status:    evalapi.ResultStatusCompleted,
		Artifacts: []evalapi.Artifact{{ID: "art-1", SessionID: "kernel-1", MIME: "image/png", Size: 3}},
	}
	fake.artifacts["art-1"] = []byte("png")
	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if ran.Version == added.Version {
		t.Fatal("the run wrote nothing")
	}
	exported, _, err := host.export(ctx, sessionID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	document, err := ipynb.Parse(exported)
	if err != nil {
		t.Fatal(err)
	}
	var inlined bool
	for _, cell := range document.Cells {
		for _, output := range cell.Outputs {
			if output.Data != nil && len(output.Data["image/png"].String()) > 0 {
				inlined = true
			}
		}
	}
	if !inlined {
		t.Fatalf("export did not inline the artifact: %s", exported)
	}
	if !strings.Contains(exported, "art-1") {
		t.Fatal("export dropped the artifact reference")
	}
	// The stored document keeps a durable reference, and the artifact's bytes are in the session's
	// artifact store, so a saved notebook keeps its image after a restart.
	stored, err := host.get(ctx, sessionID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reference *ipynb.OutputLC
	for _, cell := range stored.Document.Cells {
		for _, output := range cell.Outputs {
			if output.LC != nil && output.LC.ArtifactID != "" {
				reference = output.LC
			}
			if len(output.Data) > 0 {
				t.Fatalf("the stored document inlined the bytes: %#v", output.Data)
			}
		}
	}
	if reference == nil || reference.NotDurable {
		t.Fatalf("the stored document holds no durable reference: %#v", reference)
	}
	if _, err := host.notebookArtifact(ctx, sessionID, reference.ArtifactID); err != nil {
		t.Fatal(err)
	}
}

// TestExecutionNotebookLargeArtifactIsVolatile: an artifact past the inline limit stays a reference
// marked NotDurable, so the page never presents a link that a restart would break.
func TestExecutionNotebookLargeArtifactIsVolatile(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	created, err := host.create(ctx, sessionID, "Big", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "big()"}, -1, created.Version); err != nil {
		t.Fatal(err)
	}
	size := int64(ExecutionNotebookArtifactMaxBytes + 1)
	fake.answers["big()"] = evalapi.Result{
		Status:    evalapi.ResultStatusCompleted,
		Artifacts: []evalapi.Artifact{{ID: "big-1", SessionID: "kernel-1", MIME: "image/png", Size: size, SHA256: "sha"}},
	}
	fake.artifacts["big-1"] = bytes.Repeat([]byte("x"), int(size))

	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	cell := ran.Document.Cells[0]
	var reference *ipynb.OutputLC
	for _, output := range cell.Outputs {
		if output.LC != nil && output.LC.ArtifactID == "big-1" {
			reference = output.LC
		}
		if len(output.Data) > 0 {
			t.Fatalf("a large artifact was inlined: %d bytes", len(output.Data["image/png"].String()))
		}
	}
	if reference == nil || !reference.NotDurable || reference.Size != size {
		t.Fatalf("reference = %#v", reference)
	}
	if history := ran.Document.Extension().History; len(history) != 1 || history[0].Artifacts[0] != "big-1" {
		t.Fatalf("history = %#v", history)
	}
}

// TestExecutionNotebookCrossLanguageReplay: a notebook whose cells name different languages runs each
// cell on its own language's kernel, and a replay closes every kernel and rebuilds them clean.
func TestExecutionNotebookCrossLanguageReplay(t *testing.T) {
	_, sessionID, _, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()

	created, err := host.create(ctx, sessionID, "Mixed", "python")
	if err != nil {
		t.Fatal(err)
	}
	pythonCell := ipynb.Cell{
		CellType: ipynb.CellCode, Source: "py()",
		Metadata: ipynb.CellMetadata{LC: &ipynb.CellLC{Language: "python"}},
	}
	javascriptCell := ipynb.Cell{
		CellType: ipynb.CellCode, Source: "js()",
		Metadata: ipynb.CellMetadata{LC: &ipynb.CellLC{Language: "javascript"}},
	}
	step, err := host.addCell(ctx, sessionID, created.ID, pythonCell, -1, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	step, err = host.addCell(ctx, sessionID, created.ID, javascriptCell, -1, step.Version)
	if err != nil {
		t.Fatal(err)
	}
	if languages := step.Document.Languages(); len(languages) != 2 {
		t.Fatalf("languages = %v", languages)
	}

	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.created) != 2 {
		t.Fatalf("kernels created = %v, want one per language", fake.created)
	}
	if fake.executed[0].SessionID == fake.executed[1].SessionID {
		t.Fatal("both cells ran on the same kernel")
	}
	if fake.executed[0].Language != "python" || fake.executed[1].Language != "javascript" {
		t.Fatalf("languages = %q, %q", fake.executed[0].Language, fake.executed[1].Language)
	}
	for _, cell := range ran.Document.Cells {
		if cell.Metadata.LC.Status != ipynb.StatusOK {
			t.Fatalf("cell status = %#v", cell.Metadata.LC)
		}
	}

	replayed, err := host.run(ctx, sessionID, created.ID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.controls) != 2 {
		t.Fatalf("replay controls = %v, want one close per language", fake.controls)
	}
	if len(fake.created) != 4 {
		t.Fatalf("replay kernels = %v, want a clean kernel per language", fake.created)
	}
	if replayed.Document.Extension().ExecutionCount != 2 {
		t.Fatalf("replay execution count = %d", replayed.Document.Extension().ExecutionCount)
	}
	counts := []int{}
	for _, cell := range replayed.Document.Cells {
		if cell.ExecutionCount != nil {
			counts = append(counts, *cell.ExecutionCount)
		}
	}
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 2 {
		t.Fatalf("execution counts after replay = %v", counts)
	}
}

// TestExecutionNotebookImportRefusesOversizedDocument: an imported file larger than one stored body is
// refused before it is parsed, so a notebook cannot be smuggled past the store's limit.
func TestExecutionNotebookImportRefusesOversizedDocument(t *testing.T) {
	_, sessionID, _, _, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	oversized := strings.Repeat(" ", ExecutionNotebookMaxImportBytes+1)
	if _, err := host.importNotebook(ctx, sessionID, "Big", oversized); err == nil {
		t.Fatal("an oversized notebook was imported")
	}
}

// TestExecutionNotebookArtifactSurvivesReopen: the notebook's artifact bytes are host-owned, so a
// completed cell's image is readable after the Engine — and the kernel that produced it — is gone.
func TestExecutionNotebookArtifactSurvivesReopen(t *testing.T) {
	engine, sessionID, workspace, fake, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	created, err := host.create(ctx, sessionID, "Durable", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.addCell(ctx, sessionID, created.ID, ipynb.Cell{CellType: ipynb.CellCode, Source: "chart()"}, -1, created.Version); err != nil {
		t.Fatal(err)
	}
	fake.answers["chart()"] = evalapi.Result{
		Status:    evalapi.ResultStatusCompleted,
		Artifacts: []evalapi.Artifact{{ID: "art-1", SessionID: "kernel-1", MIME: "image/png", Size: 6, SHA256: ""}},
	}
	fake.artifacts["art-1"] = []byte("image!")

	ran, err := host.run(ctx, sessionID, created.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var artifactID string
	for _, cell := range ran.Document.Cells {
		for _, output := range cell.Outputs {
			if output.LC != nil && output.LC.ArtifactID != "" {
				artifactID = output.LC.ArtifactID
			}
		}
	}
	if artifactID == "" {
		t.Fatalf("no artifact reference: %#v", ran.Document.Cells[0].Outputs)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.StoredExecutionArtifact(ctx, sessionID, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Data) != "image!" || stored.MIME != "image/png" {
		t.Fatalf("after reopen = %#v", stored)
	}
	// The kernel that produced it is gone, so the kernel-scoped path must not be what served this.
	if _, err := reopened.ExecutionArtifact(ctx, sessionID, "kernel-1", "art-1"); err == nil {
		t.Log("kernel path also resolved; the durable read above is the one under test")
	}
}

// TestNotebookArtifactStoreDedupesAndVerifies: the same bytes are stored once, a digest that does not
// match the bytes is refused, and a file whose content no longer matches its digest is not served.
func TestNotebookArtifactStoreDedupesAndVerifies(t *testing.T) {
	engine, sessionID, _, _, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	data := []byte("image!")
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	artifact := evalapi.Artifact{ID: "art-1", MIME: "image/png", Size: int64(len(data)), SHA256: digest}

	first, err := host.putNotebookArtifact(ctx, sessionID, artifact, data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.putNotebookArtifact(ctx, sessionID, artifact, data)
	if err != nil {
		t.Fatal(err)
	}
	if first.ArtifactID != second.ArtifactID || first.SHA256 != digest {
		t.Fatalf("the same bytes were stored twice: %#v, %#v", first, second)
	}
	if _, err := host.putNotebookArtifact(ctx, sessionID, evalapi.Artifact{
		ID: "art-2", MIME: "image/png", Size: int64(len(data)), SHA256: "sha256:" + strings.Repeat("0", 64),
	}, data); err == nil {
		t.Fatal("an artifact whose digest did not match its bytes was stored")
	}
	if _, err := host.notebookArtifact(ctx, sessionID, "../escape"); err == nil {
		t.Fatal("a reference that is not a digest was read")
	}
	// A file replaced under its digest is refused rather than served.
	path := filepath.Join(engine.notebookArtifactDir(sessionID), first.ArtifactID+".bin")
	if err := os.WriteFile(path, []byte("other!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.notebookArtifact(ctx, sessionID, first.ArtifactID); err == nil {
		t.Fatal("a corrupted artifact was served")
	}
}

// TestNotebookArtifactRejectsForeignSession: a read of another session's or a path-shaped session id
// finds nothing, and a withdrawn session serves nothing.
func TestNotebookArtifactRejectsForeignSession(t *testing.T) {
	engine, sessionID, _, _, host := executionNotebookHostForTest(t)
	ctx := context.Background()
	data := []byte("image!")
	artifact := evalapi.Artifact{ID: "art-1", MIME: "image/png", Size: int64(len(data))}
	reference, err := host.putNotebookArtifact(ctx, sessionID, artifact, data)
	if err != nil {
		t.Fatal(err)
	}
	// A path-shaped identity never reaches the filesystem.
	for _, bad := range []string{"../" + sessionID, sessionID + "/..", "..", ""} {
		if _, err := host.notebookArtifact(ctx, bad, reference.ArtifactID); err == nil {
			t.Fatalf("session %q was accepted", bad)
		}
	}
	// Another real session holds its own store and cannot read this artifact.
	other, err := engine.NewSession(ctx, SessionOptions{Goal: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.notebookArtifact(ctx, other.ID, reference.ArtifactID); err == nil {
		t.Fatal("another session read the artifact")
	}
}
