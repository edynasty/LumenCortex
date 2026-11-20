package editing

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// This file owns the display form of host filesystem paths in model-facing text. Every engine
// diagnostic and every tool failure goes through it, so a workspace root, a real (symlink-resolved)
// root, a sibling directory named like the workspace, a Windows drive or UNC spelling, and a path
// that escapes through a symlink or an unresolved ancestor never reach the agent verbatim.
//
// A path that is lexically inside the workspace but whose real target is outside (or cannot be
// resolved) is fail-closed to "<outside-workspace>/<basename>": declaring it relative would name
// host content the guard never admitted. Relative paths that climb out with ".." take the same
// placeholder. The placeholder is stable so a second sanitization pass does not rewrite it.

const outsideWorkspacePlaceholder = "<outside-workspace>"

// DisplayPaths turns host paths and host-path-bearing text into workspace-relative slash paths.
// workspace and realWorkspace are the lexical workspace and its symlink-resolved root; either may be
// empty for a toolset without a workspace, and the other is derived.
type DisplayPaths struct {
	workspace     string
	realWorkspace string
	roots         []displayRoot
}

// displayRoot is one spelling of a workspace root that text sanitization recognizes.
type displayRoot struct {
	text   string
	folded bool
}

// NewDisplayPaths builds a scrubber. It resolves the real workspace with the same missing-ancestor
// logic the engine uses, so a workspace that is itself a symlink is still recognized.
func NewDisplayPaths(workspace, realWorkspace string) *DisplayPaths {
	wsClean := ""
	if workspace != "" {
		wsClean = filepath.Clean(workspace)
	}
	realClean := ""
	if realWorkspace != "" {
		realClean = filepath.Clean(realWorkspace)
	}
	if realClean == "" && wsClean != "" {
		if isWindowsDrivePath(wsClean) || isUNCPath(wsClean) {
			// A Windows/UNC spelling has no native real target on a POSIX test host; the lexical
			// root is the only sound comparison, and both spellings are recognized below.
			realClean = wsClean
		} else if real, ok := resolveDisplayReal(wsClean); ok {
			realClean = real
		}
	}
	d := &DisplayPaths{workspace: wsClean, realWorkspace: realClean}
	d.collectRoots()
	return d
}

// newEngineDisplayPaths builds a scrubber over the engine's root.
func newEngineDisplayPaths(root string) *DisplayPaths {
	if root == "" {
		return NewDisplayPaths("", "")
	}
	real := canonicalPath(root)
	clean := filepath.Clean(root)
	if real == clean {
		return NewDisplayPaths(clean, "")
	}
	return NewDisplayPaths(clean, real)
}

// collectRoots records every spelling of the workspace root that text sanitization must replace.
func (d *DisplayPaths) collectRoots() {
	if d == nil {
		return
	}
	seen := make(map[string]bool, 8)
	add := func(raw string) {
		if raw == "" {
			return
		}
		win := isWindowsDrivePath(raw) || isUNCPath(raw)
		variants := []string{raw}
		if win {
			slashed := strings.ReplaceAll(raw, `\`, "/")
			if slashed != raw {
				variants = append(variants, slashed)
			}
		} else if strings.ContainsRune(raw, '\\') {
			variants = append(variants, strings.ReplaceAll(raw, `\`, "/"))
		}
		for _, v := range variants {
			key := v
			if win {
				key = strings.ToLower(v)
			}
			if seen[key] || v == "" || v == "." || v == "/" || v == `\` {
				continue
			}
			seen[key] = true
			d.roots = append(d.roots, displayRoot{text: v, folded: win})
		}
	}
	add(d.workspace)
	add(d.realWorkspace)
}

// Path returns the display form of rawPath: a workspace-relative slash path, "." for the workspace
// root, or "<outside-workspace>/<basename>" for an absolute or escaping path.
func (d *DisplayPaths) Path(rawPath string) string {
	if rawPath == "" {
		return ""
	}
	if rawPath == "." {
		return "."
	}
	if d == nil {
		return genericDisplayPath(rawPath)
	}

	// Windows drive and UNC spellings are parsed portably, so cross-OS diagnostics and tests classify
	// them the same way on any host. No filesystem lookup is done for these.
	if targetVol, targetSub, _, ok := parseWinOrUNCRoot(rawPath); ok {
		for _, wsRoot := range []string{d.workspace, d.realWorkspace} {
			if wsRoot == "" {
				continue
			}
			wsVol, wsSub, _, wsOK := parseWinOrUNCRoot(wsRoot)
			if !wsOK || wsVol != targetVol {
				continue
			}
			relative, inside := relativeWithinSlash(wsSub, targetSub)
			if inside {
				if relative == "" {
					return "."
				}
				return relative
			}
		}
		return outsidePlaceholderForSlashPath(targetSub)
	}

	if d.workspace == "" && d.realWorkspace == "" {
		return genericDisplayPath(rawPath)
	}

	wsClean := d.workspace
	if wsClean == "" {
		wsClean = d.realWorkspace
	}
	realWS := d.realWorkspace
	if realWS == "" {
		realWS = wsClean
	}

	// A workspace written with Windows or UNC notation and a relative input is joined portably.
	if wsVol, wsSub, _, wsOK := parseWinOrUNCRoot(wsClean); wsOK && !filepath.IsAbs(rawPath) {
		slashed := strings.ReplaceAll(rawPath, `\`, "/")
		slashed = strings.TrimPrefix(slashed, "./")
		return d.Path(wsVol + wsSub + "/" + slashed)
	}

	candidate := rawPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(wsClean, candidate)
	}
	candidate = filepath.Clean(candidate)

	lexRel, lexInside := relativeWithin(wsClean, candidate)
	real, resolvable := resolveDisplayReal(candidate)
	if !resolvable {
		// An unresolved symlink or ancestor cannot prove the target is inside the workspace.
		return outsidePlaceholderForSlashPath(filepath.ToSlash(candidate))
	}
	realRel, realInside := relativeWithin(realWS, real)
	if !realInside {
		return outsidePlaceholderForSlashPath(filepath.ToSlash(candidate))
	}
	if lexInside {
		if lexRel == "" {
			return "."
		}
		return filepath.ToSlash(lexRel)
	}
	if realRel == "" {
		return "."
	}
	return filepath.ToSlash(realRel)
}

// genericDisplayPath classifies a path without a configured workspace.
func genericDisplayPath(rawPath string) string {
	if targetVol, targetSub, _, ok := parseWinOrUNCRoot(rawPath); ok {
		_ = targetVol
		return outsidePlaceholderForSlashPath(targetSub)
	}
	if filepath.IsAbs(rawPath) {
		return outsidePlaceholderForSlashPath(filepath.ToSlash(filepath.Clean(rawPath)))
	}
	cleanRel := path.Clean(filepath.ToSlash(rawPath))
	if cleanRel == ".." || strings.HasPrefix(cleanRel, "../") {
		return outsidePlaceholderForSlashPath(cleanRel)
	}
	return cleanRel
}

// Text sanitizes host filesystem paths embedded in a free-form message: a structured error rendered
// by the OS, a language server's err.Error(), or any diagnostic text. Boundaries are path-component
// aware, so a sibling such as "<root>-sibling/secret" is not mistaken for the root and is never
// rewritten to ".-sibling/secret".
func (d *DisplayPaths) Text(msg string) string {
	if msg == "" {
		return msg
	}
	if d == nil {
		return d.genericText(msg)
	}
	var out strings.Builder
	out.Grow(len(msg))
	i := 0
	for i < len(msg) {
		if !isErrTextLeftBoundary(msg, i) || isURLSchemeBefore(msg, i) {
			out.WriteByte(msg[i])
			i++
			continue
		}
		// 0. A file:// URL carries a host path in its remainder (a common LSP/go-to-file error).
		if urlEnd, matched := fileURLToken(msg, i); matched {
			out.WriteString(msg[i : i+len("file://")])
			out.WriteString(d.Path(msg[i+len("file://") : urlEnd]))
			i = urlEnd
			continue
		}
		// 1. An exact spelling of a workspace root. The root itself may contain spaces; a
		// component separator or a token boundary must follow, so a sibling never matches.
		if rootEnd, matched := d.matchRoot(msg, i); matched {
			if rootEnd >= len(msg) || isErrTextRightBoundary(msg, rootEnd) {
				out.WriteString(".")
				i = rootEnd
				continue
			}
			end := scanDisplayToken(msg, rootEnd)
			out.WriteString(d.Path(msg[i:end]))
			i = end
			continue
		}

		// 2. A Windows drive path token, portable across host OSes.
		if isWindowsDrivePath(msg[i:]) && len(msg)-i >= 3 && (msg[i+2] == '\\' || msg[i+2] == '/') {
			end := scanDisplayToken(msg, i+2)
			out.WriteString(d.Path(msg[i:end]))
			i = end
			continue
		}

		// 3. A UNC path token.
		if isUNCPath(msg[i:]) {
			end := scanDisplayToken(msg, i+2)
			if _, _, _, ok := parseWinOrUNCRoot(msg[i:end]); ok {
				out.WriteString(d.Path(msg[i:end]))
				i = end
				continue
			}
		}

		// 4. A POSIX host path token.
		if msg[i] == '/' {
			end := scanDisplayToken(msg, i)
			token := msg[i:end]
			if looksLikeHostAbsToken(token) {
				out.WriteString(d.Path(token))
				i = end
				continue
			}
		}
		out.WriteByte(msg[i])
		i++
	}
	return out.String()
}

// genericText is Text without a workspace: it still replaces absolute host tokens with a basename
// placeholder, but no root is special.
func (d *DisplayPaths) genericText(msg string) string {
	if msg == "" || !strings.ContainsAny(msg, `/\`) {
		return msg
	}
	var out strings.Builder
	out.Grow(len(msg))
	i := 0
	for i < len(msg) {
		if !isErrTextLeftBoundary(msg, i) || isURLSchemeBefore(msg, i) {
			out.WriteByte(msg[i])
			i++
			continue
		}
		if urlEnd, matched := fileURLToken(msg, i); matched {
			out.WriteString(msg[i : i+len("file://")])
			out.WriteString(genericDisplayPath(msg[i+len("file://") : urlEnd]))
			i = urlEnd
			continue
		}
		if isWindowsDrivePath(msg[i:]) && len(msg)-i >= 3 && (msg[i+2] == '\\' || msg[i+2] == '/') {
			end := scanDisplayToken(msg, i+2)
			out.WriteString(genericDisplayPath(msg[i:end]))
			i = end
			continue
		}
		if isUNCPath(msg[i:]) {
			end := scanDisplayToken(msg, i+2)
			if _, _, _, ok := parseWinOrUNCRoot(msg[i:end]); ok {
				out.WriteString(genericDisplayPath(msg[i:end]))
				i = end
				continue
			}
		}
		if msg[i] == '/' {
			end := scanDisplayToken(msg, i)
			token := msg[i:end]
			if looksLikeHostAbsToken(token) {
				out.WriteString(genericDisplayPath(token))
				i = end
				continue
			}
		}
		out.WriteByte(msg[i])
		i++
	}
	return out.String()
}

// matchRoot reports the end index of a workspace root spelling starting at i, with the required
// left boundary and a component separator or right boundary after it.
func (d *DisplayPaths) matchRoot(msg string, i int) (int, bool) {
	if d == nil {
		return 0, false
	}
	for _, root := range d.roots {
		if len(msg)-i < len(root.text) {
			continue
		}
		window := msg[i : i+len(root.text)]
		eq := window == root.text
		if !eq && root.folded {
			eq = strings.EqualFold(window, root.text)
		}
		if !eq {
			continue
		}
		after := i + len(root.text)
		if after == len(msg) || isErrTextRightBoundary(msg, after) || msg[after] == '/' || msg[after] == '\\' {
			return after, true
		}
	}
	return 0, false
}

// relativeWithin reports target relative to root and whether target is root or inside it.
func relativeWithin(root, target string) (string, bool) {
	if root == "" || target == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return rel, true
}

// relativeWithinSlash is relativeWithin for already slash-normalized, volume-stripped paths.
func relativeWithinSlash(rootSub, targetSub string) (string, bool) {
	rootFold := strings.ToLower(rootSub)
	targetFold := strings.ToLower(targetSub)
	if targetFold == rootFold {
		return "", true
	}
	prefix := rootFold
	if prefix != "/" {
		prefix += "/"
	}
	if !strings.HasPrefix(targetFold, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(targetSub[len(rootSub):], "/")
	if rel == "" {
		return "", true
	}
	return rel, true
}

// resolveDisplayReal resolves symlinks in the part of path that exists and keeps the rest as
// written. ok is false when the deepest existing ancestor is a symlink whose target cannot be
// resolved: the display must then fail closed rather than assert lexical containment.
func resolveDisplayReal(target string) (string, bool) {
	target = filepath.Clean(target)
	existing, rest := target, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return target, true
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return target, false
	}
	return filepath.Join(real, rest), true
}

func isWindowsDrivePath(p string) bool {
	return len(p) >= 2 && isASCIIAlpha(p[0]) && p[1] == ':' && (len(p) == 2 || p[2] == '\\' || p[2] == '/')
}

func isUNCPath(p string) bool {
	return (strings.HasPrefix(p, `\\`) && len(p) > 2 && p[2] != '\\') ||
		(strings.HasPrefix(p, "//") && len(p) > 2 && p[2] != '/')
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// parseWinOrUNCRoot splits a Windows drive path ("C:\a\b") or UNC path ("\\srv\share\a\b") into a
// case-folded volume key ("C:" or "//srv/share") and a clean slash-prefixed sub-path ("/a/b" or "/").
func parseWinOrUNCRoot(raw string) (volKey string, cleanSub string, rawSub string, ok bool) {
	if isWindowsDrivePath(raw) {
		volKey = strings.ToUpper(raw[:2])
		rest := strings.ReplaceAll(raw[2:], `\`, "/")
		cleanSub = path.Clean("/" + rest)
		rawSub = cleanSub
		return volKey, cleanSub, rawSub, true
	}
	if isUNCPath(raw) {
		slashed := strings.ReplaceAll(raw[2:], `\`, "/")
		parts := strings.SplitN(slashed, "/", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", "", "", false
		}
		volKey = "//" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1])
		rest := ""
		if len(parts) == 3 {
			rest = parts[2]
		}
		cleanSub = path.Clean("/" + rest)
		rawSub = cleanSub
		return volKey, cleanSub, rawSub, true
	}
	return "", "", "", false
}

func outsidePlaceholderForSlashPath(cleanSlash string) string {
	base := path.Base(strings.TrimRight(cleanSlash, "/"))
	if base == "" || base == "." || base == "/" || base == ".." {
		return outsideWorkspacePlaceholder
	}
	return outsideWorkspacePlaceholder + "/" + base
}

func isErrTextLeftBoundary(msg string, idx int) bool {
	if idx <= 0 {
		return true
	}
	switch msg[idx-1] {
	case ' ', '\t', '\n', '\r', '"', '\'', '(', '[', '{', '=', ':':
		return true
	}
	return false
}

func isErrTextRightBoundary(msg string, pos int) bool {
	if pos >= len(msg) {
		return true
	}
	switch msg[pos] {
	case ' ', '\t', '\n', '\r', '"', '\'', ')', ']', '}', ',', ';', ':':
		return true
	}
	return false
}

// scanDisplayToken scans an absolute path token from start. A space can belong to the path (a host
// directory such as "/tmp/my ws-sibling/secret.txt"), so the scanner continues across it when a word
// within two words carries a path separator; ordinary prose stops the token.
func scanDisplayToken(msg string, start int) int {
	end := scanPathWord(msg, start)
	for end < len(msg) && msg[end] == ' ' {
		continuation := 0
		probe := end + 1
		for depth := 0; depth < 2 && probe < len(msg); depth++ {
			wordEnd := scanPathWord(msg, probe)
			if strings.ContainsAny(msg[probe:wordEnd], `/\`) {
				continuation = wordEnd
				break
			}
			if wordEnd >= len(msg) || msg[wordEnd] != ' ' {
				break
			}
			probe = wordEnd + 1
		}
		if continuation == 0 {
			break
		}
		end = continuation
	}
	return end
}

// scanPathWord scans one space-free word of a path token.
func scanPathWord(msg string, start int) int {
	end := start
	for end < len(msg) && msg[end] != ' ' && !isErrTextNonSpaceBoundary(msg, end) {
		end++
	}
	return end
}

func isErrTextNonSpaceBoundary(msg string, pos int) bool {
	if pos >= len(msg) {
		return true
	}
	switch msg[pos] {
	case '\t', '\n', '\r', '"', '\'', ')', ']', '}', ',', ';', ':':
		return true
	}
	return false
}

func isURLSchemeBefore(msg string, idx int) bool {
	for _, scheme := range []string{"http:", "https:", "file:"} {
		if idx >= len(scheme) && strings.EqualFold(msg[idx-len(scheme):idx], scheme) {
			return true
		}
	}
	return false
}

// fileURLToken recognizes the remainder of a file:// URL starting at i, which is a host path even
// when it appears inside a quoted URL.
func fileURLToken(msg string, i int) (int, bool) {
	const prefix = "file://"
	if len(msg)-i < len(prefix) || !strings.EqualFold(msg[i:i+len(prefix)], prefix) {
		return 0, false
	}
	end := scanDisplayToken(msg, i+len(prefix))
	if end == i+len(prefix) {
		return 0, false
	}
	return end, true
}

func looksLikeHostAbsToken(tok string) bool {
	if strings.Count(tok, "/") >= 2 {
		return true
	}
	for _, root := range []string{"/etc", "/var", "/tmp", "/usr", "/opt", "/dev", "/System", "/Library", "/Users", "/home", "/root", "/private", "/proc", "/sys"} {
		if tok == root || strings.HasPrefix(tok, root+"/") {
			return true
		}
	}
	return false
}

// sanitizeEngineError renders an error for the engine event payload and Result.Errors. A structured
// *os.PathError or *os.LinkError is rewritten field by field (so a path containing spaces is exact),
// a joined error keeps every cause, and free-form text falls back to sanitizeHostPathsInText.
func (e *Engine) sanitizeEngineError(p *prepared, err error) string {
	if err == nil {
		return ""
	}
	var d *DisplayPaths
	if e == nil {
		d = NewDisplayPaths("", "")
	} else {
		d = newEngineDisplayPaths(e.root)
	}
	exact := map[string]string(nil)
	if p != nil {
		exact = make(map[string]string, 6)
		rel := filepath.ToSlash(p.rel)
		for _, full := range []string{p.abs, p.key} {
			if full == "" {
				continue
			}
			exact[full] = rel
			exact[filepath.ToSlash(full)] = rel
			exact[filepath.Clean(full)] = rel
		}
	}
	msg := sanitizeStructuredError(d, err, exact)
	return d.Text(msg)
}

func sanitizeStructuredError(d *DisplayPaths, err error, exact map[string]string) string {
	if err == nil {
		return ""
	}
	switch e := err.(type) {
	case *Error:
		cp := *e
		if rel, ok := exact[cp.Path]; ok {
			cp.Path = rel
		} else if rel, ok := exact[filepath.Clean(cp.Path)]; ok {
			cp.Path = rel
		} else {
			cp.Path = d.Path(cp.Path)
		}
		if cp.Detail != "" {
			cp.Detail = d.Text(cp.Detail)
		}
		if len(cp.Diagnostics) > 0 {
			diags := make([]Diagnostic, len(cp.Diagnostics))
			copy(diags, cp.Diagnostics)
			for i := range diags {
				diags[i].Path = d.Path(diags[i].Path)
				diags[i].Text = d.Text(diags[i].Text)
			}
			cp.Diagnostics = diags
		}
		return cp.Error()
	case *os.PathError:
		return e.Op + " " + displayExactOrPath(d, e.Path, exact) + ": " + sanitizeStructuredError(d, e.Err, exact)
	case *os.LinkError:
		return e.Op + " " + displayExactOrPath(d, e.Old, exact) + " " + displayExactOrPath(d, e.New, exact) +
			": " + sanitizeStructuredError(d, e.Err, exact)
	case *os.SyscallError:
		return e.Syscall + ": " + sanitizeStructuredError(d, e.Err, exact)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) > 1 {
			parts := make([]string, 0, len(causes))
			for _, cause := range causes {
				if cause == nil {
					continue
				}
				parts = append(parts, sanitizeStructuredError(d, cause, exact))
			}
			return strings.Join(parts, "\n")
		}
	}
	return d.Text(err.Error())
}

func displayExactOrPath(d *DisplayPaths, raw string, exact map[string]string) string {
	if raw == "" {
		return ""
	}
	if rel, ok := exact[raw]; ok {
		return rel
	}
	if rel, ok := exact[filepath.Clean(raw)]; ok {
		return rel
	}
	return d.Path(raw)
}

// relativizeDiagnosticPath maps a validator or lease path into the workspace's display form. An
// unresolved absolute input, a relative ".." escape and an unresolvable symlink all fail closed to
// the outside placeholder; a relative path stays as written when it cannot escape.
func (e *Engine) relativizeDiagnosticPath(rawPath string, relByKey map[string]string) string {
	if rawPath == "" {
		return ""
	}
	if rel, ok := relByKey[rawPath]; ok && rel != "" {
		return rel
	}
	// A relative diagnostic path already names workspace content; keep its meaning without any
	// filesystem call, and fail closed only when it climbs out with "..". Absolute inputs take the
	// containment and symlink checks in Path.
	if !filepath.IsAbs(rawPath) && !isWindowsDrivePath(rawPath) && !isUNCPath(rawPath) {
		cleanRel := path.Clean(filepath.ToSlash(rawPath))
		if cleanRel == ".." || strings.HasPrefix(cleanRel, "../") {
			return outsidePlaceholderForSlashPath(cleanRel)
		}
		return cleanRel
	}
	if e == nil {
		return genericDisplayPath(rawPath)
	}
	return newEngineDisplayPaths(e.root).Path(rawPath)
}

// sanitizeHostPathsInText is the free-form half of sanitizeEngineError, kept as a method so callers
// that already hold a rendered message (a nested summary, a joined error) can scrub it.
func (e *Engine) sanitizeHostPathsInText(msg string) string {
	if e == nil || msg == "" {
		return msg
	}
	return newEngineDisplayPaths(e.root).Text(msg)
}
