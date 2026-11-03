# Syncing a release into the docs

Each published bundle version gets a page in the Kamarkat docs
(`soloz-io/kamarkat`, **Versions › Releases**). The page lists every component the
version installs, where, at what version, linked to its upstream repository, with
the newest upstream release as of the release date beside it.

## When

After the version is published and the box is verified. A version that failed
to publish has no page.

## Before

- `kamarkat/` and `zero-ops/` are checked out side by side, and `zero-ops` is on
  current `main`: the sync reads `manifests/architecture/component-catalog.yaml`
  from it to link each component.
- `gh` is authenticated (the release date comes from the registry, upstream
  versions from GitHub); `helm registry login ghcr.io`.

## Run

In `kamarkat/`:

```bash
python3 scripts/sync-releases.py --fetch <version>
```

Expected, after about two minutes:

```
fetched <version>: <behind> of <total> components behind their latest release
sync-releases: <n> versions on <m> lines
```

It pulls the version's `components.json` from the bundle chart and records, as
of the day the version was published:

- the release date;
- each component's link, from its catalog `repo`;
- the newest release its repository had published, and whether the shipped
  version is that release (green) or not (red).

These are recorded once, in `data/releases/<version>.json`, so the page keeps
showing what was latest on its release date. Every page is then regenerated from
`data/releases/`.

Then:

```bash
mint validate && mint broken-links
git add -A && git commit -m "docs(releases): <version>" && git push
```

Never hand-edit `versions/releases/`, `versions/lines/` or `snippets/releases/`:
they are regenerated on every run.

## Errors

| Message | Means | Do |
| :- | :- | :- |
| `<version> is not published in the registry` | Wrong version, or the publish failed | Check the version |
| `<version> carries no inventory` | Published before inventories existed | It cannot be synced |
| `<source> has no entry in component-catalog.yaml` | The version ships a component the current catalog no longer describes | Pull `zero-ops` `main`. If the entry was removed since, the version cannot be synced; ask before dropping or keeping it |
| `catalog entry <name> has no repo for <source>` | Catalog incomplete | Add `repo` to the entry on `zero-ops` `main` |
| `no <track> version found for <source>` | Its `track`, `tag` or `repo` does not match where the project publishes | Fix the entry on `zero-ops` `main` (see the catalog header) |
| `unknown track '<x>'` | Typo in the catalog | Use one of the tracks in the catalog header |
| `no component catalog at …` | No `zero-ops` checkout beside `kamarkat` | Check one out, or pass `--catalog <path>` |
| `<version> was fetched without its release date and upstream versions` | Data file in the old shape | `--fetch <version>` again |

## Reading the page

- **Red** is a component behind its project's newest release on the release
  date, or one that cannot be shown to match it (a digest-only image, a floating
  tag). This is the list of upgrades to plan.
- **Every component has a latest version.** Third-party versions come from where
  each project publishes (`track` in the catalog); the platform's own images are
  compared with the newest image built for them before the release. A component
  whose versions cannot be found fails the sync, naming its catalog entry.
- Docker Hub records no date per tag, so for components tracked there (BusyBox,
  HAProxy, the toolbox image) latest is as of the sync, not the release date.
- The line page shows each version's date and how many components are behind.
