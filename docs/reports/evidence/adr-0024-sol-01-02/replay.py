#!/usr/bin/env python3
"""Replay the unchanged Opus corpus source, or benchmark its prebuilt renderer inputs."""

import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess


BASES = {
    "carbon": "f3f77e35c98de4647243019568ed5ec3093cf072",
    "opus": "10f135d1f904ab88c6a086319ca5820ea6b9e09c",
    "sol": None,
}
SOURCE = Path(__file__).resolve().parent.parent / "adr-0024-r7r2-final" / "inputs" / "probes"


def replace_once(text, old, new):
    assert text.count(old) == 1, old
    return text.replace(old, new)


def instrument_dump(text):
    text = replace_once(text, '"context"', '"context"\n\t"encoding/json"')
    return replace_once(text, "\t\tvar out string", r'''		if dir := os.Getenv("SOL_INPUT_DIR"); dir != "" {
			input, err := json.MarshalIndent(struct {
				Result editing.Result `json:"result"`
				Receipt editing.CommitReceipt `json:"receipt"`
			}{res, rec}, "", "  ")
			if err != nil { t.Fatal(err) }
			if err := os.WriteFile(filepath.Join(dir, name+".json"), append(input, '\n'), 0644); err != nil { t.Fatal(err) }
		}
		var out string''')


def instrument_benchmark(text):
    text = replace_once(text, '	"os"\n	"path/filepath"', '	"sort"')
    text = replace_once(text, '''func TestOpusProbeDump(t *testing.T) {
	dir := os.Getenv("OPUS_DUMP_DIR")
	if dir == "" {
		t.Skip("OPUS_DUMP_DIR not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}''', '''var solCorpusSink string

func BenchmarkSOLArchivedCorpus(t *testing.B) {''')
    start, end = text.index("\tdump :="), text.index("\t// commit renders")
    text = text[:start] + "\tvar renders []func() string\n" + text[end:]
    text = replace_once(text, '''		var out string
		if editErr != nil {
			out = set.editFailure(res, rec, editErr).Content
		} else {
			tr, _ := set.commitOutcome(res, rec, top)
			out = tr.Content
		}
		dump(name, out)''', '''		renders = append(renders, func() string {
			if editErr != nil {
				return set.editFailure(res, rec, editErr).Content
			}
			tr, _ := set.commitOutcome(res, rec, top)
			return tr.Content
		})''')
    text = replace_once(text, '''			tr, _ := set.editOutcome(&editing.Error{Code: code, Path: pathOf("refused", 0), Detail: "refused before any write", CurrentRev: "r9"})
			dump(fmt.Sprintf("%s_refusal_%s", shape, code), tr.Content)''', '''			refusal := &editing.Error{Code: code, Path: pathOf("refused", 0), Detail: "refused before any write", CurrentRev: "r9"}
			renders = append(renders, func() string {
				tr, _ := set.editOutcome(refusal)
				return tr.Content
			})''')
    text = replace_once(text, "\tfor shape, pathOf := range shapes {", '''	shapeNames := make([]string, 0, len(shapes))
	for name := range shapes { shapeNames = append(shapeNames, name) }
	sort.Strings(shapeNames)
	for _, shape := range shapeNames {
		pathOf := shapes[shape]''')
    assert text.endswith("\n}\n")
    return text[:-2] + '''	if len(renders) != 684 { t.Fatalf("got %d corpus inputs", len(renders)) }
	cpu := func() int64 {
		var r syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil { t.Fatal(err) }
		return r.Utime.Nano() + r.Stime.Nano()
	}
	t.ReportAllocs()
	t.ResetTimer()
	before := cpu()
	for t.Loop() {
		for _, render := range renders { solCorpusSink = render() }
	}
	n := float64(t.N * len(renders))
	t.ReportMetric(float64(cpu()-before)/n, "cpu-ns/payload")
	t.ReportMetric(float64(t.Elapsed().Nanoseconds())/n, "wall-ns/payload")
}
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("scratch", type=Path, help="existing scratch directory outside the checkout")
    parser.add_argument("mode", choices=("dump", "bench"))
    parser.add_argument("variant", choices=BASES)
    parser.add_argument("--capture", action="store_true", help="in dump mode, save receipts and results separately")
    args = parser.parse_args()
    scratch = args.scratch.resolve(strict=True)
    root = Path.cwd().resolve()
    assert (root / "go.mod").is_file() and root not in scratch.parents and root != scratch
    assert not args.capture or args.mode == "dump"
    run = scratch / f"replay-{args.variant}-{args.mode}"
    run.mkdir()
    source = (SOURCE / "zz_opus_new01_dump_test.go.txt").read_text(encoding="utf-8")
    if args.mode == "bench":
        source = instrument_benchmark(source)
    elif args.capture:
        source = instrument_dump(source)
    (run / "corpus_test.go").write_text(source, encoding="utf-8")
    (run / "probe_test.go").write_bytes((SOURCE / "zz_opus_new01_probe_test.go.txt").read_bytes())
    replace = {
        str(root / "internal/toolset/zz_sol_corpus_test.go"): str(run / "corpus_test.go"),
        str(root / "internal/toolset/zz_sol_corpus_probe_test.go"): str(run / "probe_test.go"),
    }
    if base := BASES[args.variant]:
        (run / "editing.go").write_bytes(subprocess.check_output(["git", "show", f"{base}:internal/toolset/editing.go"]))
        replace[str(root / "internal/toolset/editing.go")] = str(run / "editing.go")
        # The tests for removed or later helpers are not part of this historical renderer replay.
        for name in ("r7r2_final_test.go", "r7r2_sol_test.go"):
            replace[str(root / "internal/toolset" / name)] = ""
    overlay = run / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replace}), encoding="utf-8")
    cli = ["go", "test", "-overlay", str(overlay)]
    env = {}
    if args.mode == "dump":
        env["OPUS_DUMP_DIR"] = str(run / "outputs")
        if args.capture:
            (run / "inputs").mkdir()
            env["SOL_INPUT_DIR"] = str(run / "inputs")
        cli += ["-count=1", "-v", "-run", "^TestOpusProbeDump$"]
    else:
        env["GOMAXPROCS"] = "2"
        cli += ["-run", "^$", "-bench", "^BenchmarkSOLArchivedCorpus$", "-benchmem", "-benchtime=1x", "-count=3"]
    cli += ["./internal/toolset"]
    print(f"variant={args.variant} source={BASES[args.variant] or 'working tree'}", flush=True)
    print("cmd=" + shlex.join(["env", *(f"{k}={v}" for k, v in env.items()), *cli]), flush=True)
    status = subprocess.run(cli, env=os.environ | env, check=False).returncode
    print(f"exit={status}", flush=True)
    raise SystemExit(status)


if __name__ == "__main__":
    main()
