"""Label grammar used by the SOL-REV-01 evidence scripts.

A component that is exactly "…" stands for removed path components; "…" inside a component stands
for removed bytes of that component. strict: a mark may stand for nothing; lenient: at least one
component or byte; loose: every mark is ".+" over the whole path (it may cross "/")."""
import re
from functools import lru_cache

E = "…"


@lru_cache(maxsize=None)
def _component_re(pattern, strict):
    gap = "[^/]*" if strict else "[^/]+"
    return re.compile("^" + gap.join(re.escape(x) for x in pattern.split(E)) + "$", re.S)


def component(pattern, comp, strict):
    return pattern == comp if E not in pattern else _component_re(pattern, strict).match(comp) is not None


def matches(label, path, strict):
    if label == path:
        return True
    if E not in label:
        return False
    w, a = label.split("/"), path.split("/")
    if w[-1] != E and not component(w[-1], a[-1], strict):
        return False

    @lru_cache(maxsize=None)
    def walk(i, j):
        if i == len(w):
            return j == len(a)
        if w[i] == E:
            return any(walk(i + 1, e) for e in range(j if strict else j + 1, len(a) + 1))
        return j < len(a) and component(w[i], a[j], strict) and walk(i + 1, j + 1)

    return walk(0, 0)


@lru_cache(maxsize=None)
def _loose_re(label):
    return re.compile("^" + ".+".join(re.escape(x) for x in label.split(E)) + "$", re.S)


def loose(label, path):
    return label == path or (E in label and _loose_re(label).match(path) is not None)


def resolve(label, paths, mode):
    if mode == "loose":
        return [p for p in paths if loose(label, p)]
    return [p for p in paths if matches(label, p, mode == "strict")]


def display(raw):
    """Display form of a synthetic attempted path (the review cases use clean relative paths)."""
    return "<outside-workspace>/" + raw.rsplit("/", 1)[-1] if raw.startswith("../") else raw
