# Release gates

Every gate `scripts/package/publish.sh` runs, in order. Each one exists because
the defect it refuses once reached a published release while rendering
perfectly. When one fails, fix the cause on `main` and release again; never
patch the bundle or skip the gate.

| # | Gate | Fails with | Means | Fix |
| :- | :- | :- | :- | :- |
| 1 | Up to date | `this tree is N commit(s) behind its upstream` | A CI bot pinned an image digest after your push | `git pull --rebase`, run again |
| 2 | Semver | `'<v>' is not semver` | A chart version must be requestable | Use `X.Y.Z` or `X.Y.Z-rc.N` |
| 3 | Helm 3 | `no Helm 3 available` | Helm 4 breaks kustomize's chart inflation | Install Helm 3 (`scripts/dev/helm3.sh`) |
| 4 | Component charts | `component-chart.sh` exits 3 | A component's source reaches outside its own directory | Move the shared file into the component, or make it its own component |
| 5 | Packaging | `<name>: kustomize build failed` / `renders no objects; refusing` | A packaged path does not build, or builds nothing | Fix the kustomization |
| 6 | Distribution | `verify-distribution.py` reports a difference | The one-artefact bundle renders a component differently from its own chart | Fix the distribution or the component's values |
| 7 | Portability | `bundle-portability.sh` | The bundle reaches back into the platform's repository | Remove the Git reference; everything ships in the bundle (ADR-063) |
| 8 | Equivalence | `verify-equivalence.py` | A packaged chart renders different objects from its source | Fix the packaging, not the comparison |
| 9 | Application specs | `application specs: N problem(s)` | An Application would be refused or manage nothing | See below |
| 10 | Support matrix | `support-matrix.sh` | A supported combination fails to render, or an unsupported one is not refused | Fix the chart, or `supportedMatrix` in `environment-manager/values.yaml` |
| 11 | Inventory | `have no catalog entry` (exit 3) | An artefact ships that the catalog does not describe | Add a catalog entry (see SKILL.md, Prepare 4) |
| 12 | Operator defaults | `… is reconciled by <operator> <v>, but its defaults were read at <v>` | An operator was bumped | Re-read its image defaults at the new tag; update `OPERATED` in `inventory.py` |
| 13 | Seed parity | `Day-0 seeds argo-cd <v> but the bundle reconciles <v>` (or cert-manager) | The CLI installs a different version at Day-0 than GitOps then adopts | Align the CLI's version constants with the bundle |
| 14 | Not yet published | `version … is already published` | Some chart already exists at this version | Release the next version |

## Gate 9: application specs

Every supported environment and provider combination is rendered twice: once with
default values, and once with every optional component on (every boolean
`enabled` in the bundle's values, plus a burst pool). Each Application is checked:

1. one source type only;
2. **the chart it names is in the bundle** — `enables component '<x>', which the
   distribution does not carry` means a component a box can deploy is not
   packaged. Package it (SKILL.md, Prepare 5);
3. it resolves to at least one object;
4. no runtime dependency on the platform's Git;
5. it renders for the topology given;
6. the spec is structurally valid.

A failure only in the "every optional component on" render means the defect is
behind a switch no default box turns on. It is still a defect: the first box
that turns it on gets an Application that renders nothing and reports Healthy.

## Release check: every component behind latest has a reason

`kakt release check` runs this after the gates above. It looks up each
component's newest upstream release and compares it with what the bundle ships.

| Fails with | Means | Fix |
| :- | :- | :- |
| `could not reach an upstream API` | A network or GitHub API failure, not a catalog problem | Run the check again |
| `looked up something that does not exist` (`… does not exist`) | A catalog `repo` or `match` names something upstream does not have | Correct the name in the catalog entry |
| `looked up something that does not exist` (`<package> … does not exist`, `has no image in the registry`) for a platform image | Its build has not published under the current name, e.g. after a repository rename, or its last run failed | `gh workflow run <image>.yml --ref main`, then `git pull --rebase` and run again |
| `<component> [<v>]: no reason` | The release ships a version behind its latest release, and no `behind` item names it | Check the pin first (below), then add the item |
| `its reason says it changes in <target>, but <v> still ships it` | A promise this release breaks | Land the change, or give the real status and a later target |

### Check the pin before writing a reason

A reason explains a version someone chose. Before adding one, find where the
version is pinned and confirm it was chosen. Two versions of one component on
one cluster (`v2.14.0, v2.2.0`), or an image `tag` in values that differs from
the chart's own version, is a stale override left behind when the chart moved.
Remove the override so the image follows the chart; do not explain it.
