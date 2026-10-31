#!/usr/bin/env python3
"""Split a rendered component into verbatim content and templated objects.

The spoke catalogue is reconciled by every spoke, and a few of its fields must
differ per spoke: which Infisical path holds its credentials, which DNS zone it
may write, which backup prefix it owns. As a Kustomize source those were patches
on the Application. A published bundle makes the source a Helm chart, and ArgoCD
permits one source type per source, so the patches have nowhere to live and the
fields must come from chart values instead.

Objects named in the field declaration are emitted as Helm templates with those
paths substituted. Everything else -- the CRDs and the bulk of the catalogue --
stays verbatim under files/, because templating content that contains Go
delimiters of its own has silently blanked a credential here before.

An object named in the declaration that the render does not contain is an error,
not a no-op: a Kustomize patch whose target is missing fails the build, and
losing that would turn a stale declaration into a spoke reading another spoke's
secrets.

Delimiters already in a templated object are escaped before it becomes a
template. An ExternalSecret's template block legitimately contains `{{ .password }}`,
which is External Secrets Operator's syntax for the fetched value and not Helm's
-- rendered as Helm it becomes the empty string, and the credential is blanked
with no error anywhere. That is the whole reason verbatim content lives under
files/; an object that must be templated cannot, so its own delimiters are
protected instead.

A field either assigns a whole value (`value:`), substitutes a literal inside
one (`replace: {from:, with:}`) for values buried in an opaque blob no path can
address, or substitutes it throughout the object (`replaceAll: {from:, with:}`)
for objects that carry the same per-instance value in every field. A `value:`
field marked `type: int` renders as an integer, not a quoted string (replica
counts that differ per environment).

A spec may also carry `when:`, a Helm expression the whole object is gated on: it
is emitted inside `{{- if <expr> }}`, so a box whose values do not satisfy it gets
no such object at all. That is for an object which cannot be configured partially
-- an authorization boundary whose audience the box has not declared yet is the
case this was built for. Rendering it with an empty audience would admit every
client of the issuer, and `required` cannot express it either: the value is minted
by the release being packaged, so no value exists at package time and refusing to
render the catalogue takes every other object in it down with the one.

A component may render into several files. All of them are passed at once and a
declared object must appear in exactly one of them: checked per file, a
declaration would have to name objects that live in a different file, so every
multi-file component would fail on its first entry.

Usage: templated-fields.py <declaration.yaml> <chart-dir> <rendered.yaml>...
"""
import os
import re
import sys

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required")


def protect(node):
    """Escape delimiters that belong to something other than Helm.

    Returned through Helm's own `{{ "..." }}` so the literal survives rendering.
    """
    if isinstance(node, dict):
        return {k: protect(v) for k, v in node.items()}
    if isinstance(node, list):
        return [protect(v) for v in node]
    if isinstance(node, str) and ("{{" in node or "}}" in node):
        # One pass: replacing "{{" first would insert a "}}" that the second
        # replacement then rewrites, nesting the escape inside itself.
        return re.sub(r"\{\{|\}\}", lambda m: '{{ "%s" }}' % m.group(0), node)
    return node


def resolve(doc, path):
    """Return (container, key) for a nested path, indexing lists by integer."""
    node = doc
    for step in path[:-1]:
        node = node[step]
    return node, path[-1]


def assign(doc, path, value):
    """Set a nested path, indexing lists by integer, as the patches did."""
    node, key = resolve(doc, path)
    node[key] = value


def substitute_all(node, literal, replacement):
    """Replace a literal in every string under a node, returning a count.

    Some objects carry a per-instance value in every field rather than in one:
    a Certificate's dnsNames are all the same domain, and so are a ClusterIssuer's
    solver zones. Naming each index would make the declaration a list of numbers
    that silently goes stale when an entry is inserted -- the opposite of the
    reviewable contract this file is meant to be. Naming the object and the
    literal says the same thing and survives the edit.
    """
    if isinstance(node, dict):
        # apiVersion and kind identify the TYPE, never the instance, and a group
        # that contains the vendor's domain is still a group: ops.nutgraf.in is
        # the API this platform serves, exactly as cluster.x-k8s.io is upstream's.
        # Substituting there rewrote apiVersion to ops.<tenant domain>/v1alpha1
        # and ArgoCD refused the object -- "could not find version v1alpha1 of
        # ops./HubEnvironment" -- after the bundle had published and six
        # boundaries had deployed.
        #
        # metadata.name for the same reason the autoscaler's name was left alone:
        # renaming an object by value makes the resource ArgoCD prunes depend on
        # that value, so a change deletes and recreates rather than updates.
        return sum(substitute_all_in(node, k, literal, replacement)
                   for k in list(node) if k not in ("apiVersion", "kind"))
    if isinstance(node, list):
        return sum(substitute_all_in(node, i, literal, replacement) for i in range(len(node)))
    return 0


def substitute_all_in(container, key, literal, replacement):
    if key == "metadata" and isinstance(container.get(key), dict):
        meta = container[key]
        return sum(substitute_all_in(meta, k, literal, replacement)
                   for k in list(meta) if k != "name")
    v = container[key]
    if isinstance(v, str):
        if literal in v:
            container[key] = v.replace(literal, replacement)
            return 1
        return 0
    return substitute_all(v, literal, replacement)


def substitute(doc, path, literal, replacement):
    """Replace a literal inside the string at a path, leaving the rest verbatim.

    Some per-instance values are not addressable as a field. agentgateway's
    config is one opaque `config.yaml` string inside a ConfigMap, and the hub's
    domain appears three times inside it -- as the OIDC issuer, the required
    audience, and the JWKS URL. Assigning the whole blob would mean carrying a
    two-hundred-line config in the declaration, which stops being reviewable and
    so stops being the contract this file exists to be.

    Refuses a literal that is not present. A declaration that silently matches
    nothing is how a tenant's cluster keeps pointing at the platform's identity
    provider while the packager reports success -- which is the defect this was
    added for.
    """
    node, key = resolve(doc, path)
    text = node[key]
    if not isinstance(text, str):
        raise TypeError(f"not a string, cannot substitute: {type(text).__name__}")
    if literal not in text:
        raise KeyError(f"literal {literal!r} not present")
    node[key] = text.replace(literal, replacement)



# A value carrying a Go template is emitted double-quoted, always.
#
# The dumper cannot know what the template will render to, and PyYAML decides
# quoting from the text it is given -- so `support-agent:{{ .Values.global.hubDomain }}`
# looks like a legal plain scalar and is emitted bare. It renders to
# `support-agent:` when the value is empty, which is a second colon on the line
# and invalid YAML, and helm reports it as "mapping values are not allowed in
# this context" pointing at a line that is fine.
#
# Quoting costs nothing anywhere else: helm substitutes inside the quotes, and a
# quoted scalar is the same value.
class _Dumper(yaml.SafeDumper):
    pass


def _templated_str(dumper, data):
    # Single-line templated scalars only. A MULTI-LINE value carrying a template
    # -- an embedded script, a patch body, a Composition's inline manifest --
    # must keep block style: forcing it to a double-quoted scalar re-encodes
    # every newline as \n, and what helm then renders is one long line that is
    # no longer the YAML the component meant to emit. That broke
    # platform-tenant-platform, whose Composition templates a hostAPI inside an
    # inline manifest block.
    # SINGLE quotes, not double. A templated value routinely contains double
    # quotes of its own -- `{{ "{{" }}` is how a Helm template emits a literal
    # Go-template brace, and the tenant-platform Composition does exactly that
    # inside a redis URL. Double-quoting re-escapes those as \" and helm then
    # fails to parse its own template with `unexpected "\\" in command`.
    # Single-quoted YAML is verbatim apart from ' itself, which PyYAML doubles
    # correctly.
    #
    # Multi-line values keep block style: a folded or quoted multi-line scalar
    # re-encodes newlines and what helm renders is no longer the YAML the
    # component meant to emit.
    style = "'" if "{{" in data and "\n" not in data else None
    if "{{" in data and "\n" in data:
        # Force the literal block style that comment demands. PyYAML refuses `|`
        # for text with trailing spaces or a leading newline and falls back to a
        # double-quoted scalar, which escapes the template's own quotes (`eq
        # .Values.x "dev"`) and helm then cannot parse it. Trailing whitespace
        # and a leading blank line carry no meaning in the configs these are.
        data = "\n".join(line.rstrip() for line in data.split("\n")).lstrip("\n")
        style = "|"
    return dumper.represent_scalar("tag:yaml.org,2002:str", data, style=style)


_Dumper.add_representer(str, _templated_str)


# An integer field (`type: int`) carries a template that must render bare:
# `replicas: '{{ ... }}'` is a string and the API server refuses it. The dumper
# cannot emit `{{` unquoted (it reads as a flow mapping), so the field is
# assigned a plain placeholder and the template is put back after dumping. Helm
# renders the template before anything parses the YAML, so the result is an
# integer.
_INT_TEMPLATES = {}


def int_placeholder(template):
    key = f"ZZTEMPLATEDINT{len(_INT_TEMPLATES)}ZZ"
    _INT_TEMPLATES[key] = template
    return key


def _dump(doc):
    text = yaml.dump(doc, Dumper=_Dumper, sort_keys=False, width=10**6)
    for key, template in _INT_TEMPLATES.items():
        text = text.replace(key, template)
    return text


def _dump_all(docs, handle):
    yaml.dump_all(docs, handle, Dumper=_Dumper, sort_keys=False, width=10**6)



# Every `.Values.global.*` the substituted values reference, as a nested default.
#
# A templated object that reads .Values.global.dns.ownerId renders NOTHING when
# the chart carries no default for it: `global.dns` is a nil map and helm fails
# the whole render, which component-chart.sh then reports as "packaged chart
# renders no objects" -- naming the symptom and not the cause. That cost an hour
# the first time and would cost it again for the next global anyone adds.
#
# Derived from what was actually emitted rather than listed by hand, so a new
# global is defaulted by the commit that introduces it.
def global_defaults(emitted):
    root = {}
    for text in emitted:
        for path in re.findall(r"\.Values\.global\.([A-Za-z0-9_.]+)", text):
            node = root
            parts = path.split(".")
            for key in parts[:-1]:
                node = node.setdefault(key, {})
                if not isinstance(node, dict):
                    break
            else:
                node.setdefault(parts[-1], "")
    return {"global": root} if root else {}


def main() -> int:
    if len(sys.argv) < 4:
        print(__doc__)
        return 2
    declaration, chart, rendered_files = sys.argv[1], sys.argv[2], sys.argv[3:]

    by_file = {}
    docs = []
    for rendered in rendered_files:
        with open(rendered) as handle:
            these = [d for d in yaml.safe_load_all(handle) if d]
        by_file[rendered] = these
        docs.extend(these)
    with open(declaration) as handle:
        wanted = yaml.safe_load(handle) or []

    index = {}
    for doc in docs:
        index[(doc.get("kind"), (doc.get("metadata") or {}).get("name"))] = doc

    templated, verbatim, missing = [], [], []
    claimed = set()
    # Documents already escaped, and those already collected for output.
    #
    # A declaration file may name one object more than once -- the spoke
    # catalogue's ClusterSecretStore is declared twice, once for the environment
    # slug and once for the hub's address, because they are separate decisions
    # with separate reasons. Both are legitimate and the file reads better for it.
    #
    # Processing them independently was not. Each pass re-escaped the whole
    # document, so the SECOND declaration protected the live Helm expression the
    # FIRST had just assigned: the shipped chart carried
    #   environmentSlug: '{{ "{{" }} .Values.global.environmentSlug {{ "}}" }}'
    # which renders to that expression as literal text. Every spoke therefore got
    # a ClusterSecretStore whose environmentSlug was the string
    # "{{ .Values.global.environmentSlug }}", external-secrets rejected the store
    # with InvalidProviderConfig, and every ExternalSecret behind it stayed
    # Progressing forever -- the spoke's S3 credential among them, so its database
    # could not authenticate to object storage and never backed up.
    #
    # Each pass also appended the document again, so the object was emitted twice.
    protected = set()
    collected = set()
    # kind/name -> the Helm expression the object is gated on, if any.
    guards = {}
    conflicts = []
    for spec in wanted:
        key = (spec["kind"], spec["name"])
        doc = index.get(key)
        if doc is None:
            missing.append(f"{spec['kind']}/{spec['name']}")
            continue
        # Escape first, then assign: the values being assigned are Helm
        # expressions and must stay live. ONCE per document, however many
        # declarations name it -- see above.
        if id(doc) not in protected:
            protected.add(id(doc))
            for field_name in list(doc):
                doc[field_name] = protect(doc[field_name])
        for field in spec.get("fields") or []:
            try:
                if "replaceAll" in field:
                    # No path: the literal is replaced wherever it appears in the
                    # object. Refused when absent, like the addressed form.
                    n = substitute_all(doc, field["replaceAll"]["from"],
                                       field["replaceAll"]["with"])
                    if n == 0:
                        raise KeyError(
                            f"literal {field['replaceAll']['from']!r} not present anywhere")
                elif "replace" in field:
                    substitute(doc, field["path"],
                               field["replace"]["from"], field["replace"]["with"])
                elif field.get("type") == "int":
                    assign(doc, field["path"], int_placeholder(field["value"]))
                else:
                    assign(doc, field["path"], field["value"])
            except (KeyError, IndexError, TypeError) as exc:
                # A replaceAll field has no path, so describing one by its path
                # raised a second KeyError inside the handler and the packager
                # died with a traceback instead of naming the declaration.
                where = ('.'.join(map(str, field["path"]))
                         if "path" in field else "anywhere in the object")
                missing.append(f"{spec['kind']}/{spec['name']} {where}: {exc}")
        if "when" in spec:
            previous = guards.get(id(doc))
            if previous is not None and previous != spec["when"]:
                conflicts.append(
                    f"{spec['kind']}/{spec['name']}: guarded by two different "
                    f"conditions, {previous!r} and {spec['when']!r}")
            guards[id(doc)] = spec["when"]
        claimed.add(key)
        if id(doc) not in collected:
            collected.add(id(doc))
            templated.append(doc)

    if conflicts:
        print("the templated-field declaration gates one object on two conditions:",
              file=sys.stderr)
        for item in conflicts:
            print(f"  {item}", file=sys.stderr)
        print("Which one wins would be the packager's choice to make, and it is not "
              "the packager's to make.", file=sys.stderr)
        return 1

    if missing:
        print("the templated-field declaration names content this component does "
              "not render:", file=sys.stderr)
        for item in missing:
            print(f"  {item}", file=sys.stderr)
        print("A declaration that no longer matches is how a spoke ends up reading "
              "another spoke's secrets.", file=sys.stderr)
        return 1

    # By kind and name, not object identity: a document that was escaped is a
    # different object, and identity matching would leave it in both halves.
    remaining = {}
    for rendered, these in by_file.items():
        keep = [d for d in these
                if (d.get("kind"), (d.get("metadata") or {}).get("name")) not in claimed]
        remaining[rendered] = keep
        verbatim.extend(keep)

    os.makedirs(os.path.join(chart, "templates"), exist_ok=True)
    with open(os.path.join(chart, "templates", "templated.yaml"), "w") as handle:
        handle.write(
            "{{- /*\n"
            "Objects whose content differs per spoke. Everything else in this\n"
            "component is emitted verbatim from files/ by content.yaml.\n"
            "*/ -}}\n"
        )
        for doc in templated:
            expr = guards.get(id(doc))
            if expr:
                # The separator sits INSIDE the guard. Outside it, a gated-out
                # object leaves a stray `---` behind, and a document of nothing
                # between two separators is a null document that kustomize and
                # helm disagree about.
                handle.write("{{- if %s }}\n---\n%s{{- end }}\n"
                             % (expr, _dump(doc)))
            else:
                handle.write("---\n" + _dump(doc))

    for rendered, keep in remaining.items():
        with open(rendered, "w") as handle:
            _dump_all(keep, handle)

    # Defaults for every global the templated objects read, MERGED into whatever
    # the chart already has. Without them the chart renders nothing at all, and
    # the packager reports that as "renders no objects" -- the symptom, not the
    # cause.
    defaults = global_defaults(
        [_dump(doc) for doc in templated] + list(guards.values()))
    if defaults:
        values_path = os.path.join(chart, "values.yaml")
        existing = {}
        if os.path.exists(values_path):
            with open(values_path) as handle:
                existing = yaml.safe_load(handle) or {}
        merged = dict(existing)
        merged["global"] = {**defaults["global"], **(existing.get("global") or {})}
        with open(values_path, "w") as handle:
            handle.write(
                "# Defaults for the globals this component's templated fields read.\n"
                "# Written at package time from what was actually emitted, so a new\n"
                "# global is defaulted by the commit that introduces it. A chart with\n"
                "# no default renders nothing: the lookup hits a nil map and helm\n"
                "# fails the whole render.\n"
            )
            yaml.safe_dump(merged, handle, sort_keys=False)

    print(f"  {len(templated)} object(s) templated, {len(verbatim)} verbatim")
    return 0


if __name__ == "__main__":
    sys.exit(main())
