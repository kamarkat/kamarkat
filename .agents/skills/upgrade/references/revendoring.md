# Re-vendoring a chart render

A vendored render is an upstream artefact plus, usually, some local
transformation. Replacing one is safe exactly to the extent that you know what
that transformation is — and the header rarely tells you all of it.

So the work is in this order: **classify the file, reproduce the version you are
on, then replace it.** Reproducing first is what turns a guess into a change you
can defend.

## Contents

- [Classify the file first](#classify-the-file-first)
- [Reproduce before you replace](#reproduce-before-you-replace)
- [Finding an undocumented normaliser](#finding-an-undocumented-normaliser)
- [Check whether a header parameter changes anything](#check-whether-a-header-parameter-changes-anything)
- [Re-applying hand patches](#re-applying-hand-patches)
- [Verifying the result](#verifying-the-result)
- [When to stop](#when-to-stop)

## Classify the file first

Three kinds of file hide behind the same `.yaml`, and they carry very different
risk:

| Kind | How it looks | What replacing it is |
| :- | :- | :- |
| **Reproducible render** | upstream output plus at most a mechanical transform (a namespace rename, a serialisation pass) | a version bump — safe once reproduced |
| **Render plus patches** | upstream output with hand-added containers, env, replicas and comments explaining incidents | a re-application, which needs verification on a real cluster |
| **Hand-authored manifest** | assembled by someone from upstream's shape, no provenance header, far smaller than upstream's equivalent | a re-derivation, which is a design change and not an upgrade step |

Tell them apart by reproducing (below). The diff against upstream at the version
you are *currently on* is the classification: empty means reproducible, small
and structural means patches, large means hand-authored.

A hand-authored manifest is the one to escalate rather than edit. Upstream's
equivalent may configure through fifty environment variables where the local
copy configures through five, and the missing ones are not absent by choice —
they were never needed by the older version. Bumping its image produces a new
binary reading an old spec.

## Reproduce before you replace

The method, and it is the single highest-value step in the whole job:

1. Fetch the upstream artefact **at the version currently pinned**.
2. Reverse any transformation you already know about (undo a namespace rename,
   strip a header).
3. Diff against the vendored file.

A **zero diff** means you now know the entire pipeline and can run it at the new
version with confidence. Anything else is the local customisation, itemised for
free — which is exactly what you need and exactly what nobody wrote down.

Do this even when the header looks complete. Headers record intent; they omit
post-processing, hand patches and comments added later.

```bash
# example shape: ours, with a known rename reversed, against upstream at the pinned version
curl -fsSLo upstream-OLD.yaml <release-url-at-current-version>
sed 's/local-namespace/upstream-namespace/g' <vendored-file> > ours-reversed.yaml
diff upstream-OLD.yaml ours-reversed.yaml | wc -l     # 0 means you have the whole pipeline
```

## Finding an undocumented normaliser

If the diff is entirely cosmetic — comments stripped, quotes normalised, long
strings wrapped differently — the file went through a serialisation pass that
nobody recorded. Recover its parameters by trying candidates and diffing; this
converges in a handful of attempts:

```bash
for w in 80 100 120 150 160 180 200; do
  # re-serialise with width=$w, then diff against the vendored file
  echo "width=$w diff=$(... | diff - <vendored> | wc -l)"
done
```

A zero at one setting gives you the exact pipeline. Record it in the file's
header once you know it, so the next person does not repeat the search.

## Check whether a header parameter changes anything

A provenance header naming a parameter is not proof the output depends on it.
Render at the old value and the new value and diff:

```bash
for v in OLD NEW; do helm template ... --kube-version $v > r-$v.yaml; done
diff r-OLD.yaml r-NEW.yaml
```

Zero means the vendored file carries nothing specific to the old value, so the
upgrade needs no re-render for that parameter at all. This can dissolve a large
part of a planned change, so it is worth doing before any of the work.

## Re-applying hand patches

When the file is a render plus patches:

- **Patch textually, not through a load/dump cycle.** A YAML round-trip deletes
  every comment, and in a vendored render those comments are often the only
  record of the incident the patch exists to prevent. Losing them is worse than
  the version being stale.
- **Assert each patch applied exactly once.** A patch whose anchor moved between
  versions silently does nothing, and the result looks like a clean re-vendor.
  Count the applications and fail when the count is not what you expect.
- **Check each patch is still a patch.** A value that differs from the old
  upstream default may *be* the new upstream default; re-applying it is harmless
  but re-applying its comment as if it were local is misleading. Diff against
  the new render, not against memory.
- **Confirm the anchors exist in the new version before starting.** List the
  containers, init containers and keys your patches target. If one is gone, the
  patch needs rethinking rather than relocating.

## Verifying the result

Mechanical checks that catch most of what goes wrong:

- the object set — kinds and names, before and after, with removals and
  additions listed explicitly
- objects that must be **absent** (a ConfigMap owned by another renderer, a
  Secret whose material must not be committed)
- each patch present in the output, read back from the parsed document rather
  than grepped
- the image references, including that every digest matches its tag
- the whole file parses, and any digest-header validator passes

What these cannot tell you is whether the component still works. For anything on
the bootstrap path — the CNI above all — the local cluster test is the real gate,
and the re-vendor should land before it rather than after.

## When to stop

Stop and say so when:

- the file is hand-authored and upstream's equivalent has a materially different
  shape — that is a design decision about ownership, not a bump
- a patch's anchor no longer exists and the replacement is not obvious
- the normaliser will not converge, and the gain is a patch release

Stopping is cheap; a silently wrong render of a bootstrap component is not. Say
precisely what you established — the base command, the values, the patch list,
the remaining unknown — so the decision is someone else's to make on evidence
rather than theirs to rediscover.
