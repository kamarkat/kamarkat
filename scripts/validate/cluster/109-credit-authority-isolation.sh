#!/usr/bin/env bash
# ADR-109: the Credit Authority's isolation and guarantees, LIVE. The rendered
# half is preflight/109-credit-authority-isolation.sh; both gate the capability.
#
# The Credit Authority runs on the HUB (platform-credits) and is reached only
# through the hub Gateway at credits.<hubDomain>. Read-only: every probe is a
# connection attempt from a pod that already runs, and changes nothing.
#
#   X1  it is Ready and its provisioner exited 0
#   X2  the ledger is bound to 127.0.0.1, 3000 is the pod's only port and the
#       Service's only port, and the pod is not on the host network
#   X3  ingress is the hub Gateway's identity only, on 3000, and the route
#       reaches the gateway and nothing else
#   X4  the admitted caller is the declaring application's own service client,
#       and its organisation that application's -- the binding traces to the one
#       declaration
#   X5  nothing in its namespace grants pods/exec or pods/portforward, and no
#       service account outside the platform's own controllers can do either in
#       it -- asked of the API server, so cluster-wide bindings count: either
#       reaches the ledger's loopback past the gateway
#   X6  from another hub namespace: the ledger's port and the gateway's port on
#       the pod's address do not connect (loopback; Gateway identity only); from
#       the declaring application's SDK on its spoke, credits.<hubDomain>
#       answers through the Gateway
#   X7  with every token below set, the guarantees and isolation suites
#       (test/e2e/credit-authority) pass through credits.<hubDomain>:
#         CREDITS_TOKEN               the declaring application's machine token
#         CREDITS_ID_TOKEN            an ID token from a real login
#         CREDITS_OTHER_APP_TOKEN     another application's machine token
#         CREDITS_OTHER_TENANT_TOKEN  a token carrying another organisation's claim
#         CREDITS_EXPIRED_TOKEN       an expired token of the declaring application
#       A missing token, or any test that skips, is a failure to prove, not a
#       pass. The guarantees suite covers atomicity, concurrent spends,
#       idempotency and the refused opt-outs of the balance check.
#
# ACTIVATION EVIDENCE. X7 writes its record -- when, where, the deployed image
# digests, the admitted caller and every test's verdict -- to
# docs/evidence/109-credit-authority/, passed or not. The capability is
# activated only on a record whose X7 passed (ADR-109 §7).

# The platform's own controllers, which hold cluster-wide rights by design and
# are the "platform operators" ADR-109 §4 names. Anything else able to exec
# into or port-forward to the Credit Authority fails X5.
CREDITS_EXEC_ALLOWED=(
    system:serviceaccount:platform-ops:argocd-application-controller
    system:serviceaccount:platform-ops:argocd-server
)

validate_adr109_credit_authority() {
    section "ADR-109 Credit Authority isolation and guarantees (live, hub)"
    local ns=platform-credits

    if ! kc -n "$ns" get deployment credit-authority >/dev/null; then
        note "the Credit Authority is not enabled on this box (no deployment in $ns)"
        return 0
    fi

    local out line
    out=$(kc -n "$ns" get pods,svc,ciliumnetworkpolicies,roles,rolebindings,secrets,configmaps -o json | python3 -c '
import json, sys
items = json.load(sys.stdin)["items"]
kind = lambda k: [i for i in items if i["kind"] == k]
def check(ok, good, bad): print(("PASS " + good) if ok else ("FAIL " + bad))

pods = [p for p in kind("Pod") if p["metadata"].get("labels", {}).get("app.kubernetes.io/name") == "credit-authority"]
ready = [p for p in pods if any(c["type"] == "Ready" and c["status"] == "True" for c in p["status"].get("conditions", []))]
prov = [s for p in pods for s in p["status"].get("initContainerStatuses", []) if s["name"] == "provision"]
done = [s for s in prov if (s.get("state", {}).get("terminated") or {}).get("exitCode") == 0]
check(ready and len(done) == len(pods), "X1 Ready, the ledger provisioned",
      f"X1 {len(ready)}/{len(pods)} pods Ready, {len(done)} provisioned -- the Credit Authority is unavailable")

shape_ok = bool(pods)
for p in pods:
    spec = p["spec"]
    inits = spec.get("initContainers", [])
    ledger = next((c for c in inits if c["name"] == "ledger"), {})
    args = ledger.get("args", [])
    bind = args[args.index("--bind") + 1] if "--bind" in args else ""
    ports = [pt["containerPort"] for c in inits + spec["containers"] for pt in c.get("ports", [])]
    shape_ok = shape_ok and bind == "127.0.0.1:3068" and ports == [3000] and not spec.get("hostNetwork")
svc_ports = [pt["port"] for s in kind("Service") for pt in s["spec"].get("ports", [])]
check(shape_ok and svc_ports == [3000], "X2 the ledger is on loopback, 3000 is the only port",
      f"X2 pod or Service exposes more than the gateway (service ports {svc_ports})")

pols = kind("CiliumNetworkPolicy")
rules = [r for c in pols for r in c["spec"].get("ingress", [])]
sources = sorted({e for r in rules for e in r.get("fromEntities", [])})
endpoints = [r for r in rules if r.get("fromEndpoints")]
ports = {pt["port"] for r in rules for tp in r.get("toPorts", []) for pt in tp.get("ports", [])}
check(pols and sources == ["ingress"] and not endpoints and ports == {"3000"},
      "X3 ingress is the hub Gateway only, on 3000",
      f"X3 ingress wrong: entities={sources} endpoint rules={len(endpoints)} ports={sorted(ports)}")

def grants(rule):
    return any(r in ("pods/exec", "pods/portforward", "*", "pods/*") for r in rule.get("resources", []))
risky = [r["metadata"]["name"] for r in kind("Role") if any(grants(x) for x in r.get("rules", []))]
bound = [b["metadata"]["name"] for b in kind("RoleBinding") if b["roleRef"]["name"] in risky]
check(not bound, "X5 nothing in the namespace grants pods/exec or pods/portforward",
      f"X5 bindings granting exec or port-forward, which reach the ledger past the gateway: {bound}")
')
    while IFS= read -r line; do
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
        esac
    done <<< "$out"

    local backends
    backends=$(kc -n "$ns" get httproute credit-authority -o jsonpath='{range .spec.rules[*].backendRefs[*]}{.name}:{.port} {end}')
    if [[ "$backends" == "credit-authority:3000 " ]]; then
        pass "X3 the route reaches the gateway and nothing else"
    else
        hard_fail "X3 the route's backends are '${backends}', not the gateway alone"
    fi

    # The caller, traced to the declaration: the application whose
    # CreditAuthorityOwner condition is True, and its own service identity on
    # its spoke.
    local owner admitted admitted_org declared declared_org app cell tenant
    owner=$(kc get ainativesaas -o json | python3 -c '
import json, sys
for a in json.load(sys.stdin)["items"]:
    conds = {c["type"]: c["status"] for c in a.get("status", {}).get("conditions", [])}
    if conds.get("CreditAuthorityOwner") == "True":
        s = a["spec"]
        print(s["tenantId"], s["appId"], s["clusterName"], a.get("status", {}).get("identity", {}).get("zitadelOrgId", "")); break
')
    read -r tenant app cell declared_org <<< "$owner"
    admitted=$(kc -n "$ns" get secret credit-authority-caller -o jsonpath='{.data.CREDITS_CALLER_CLIENT_ID}' | base64 -d)
    admitted_org=$(kc -n "$ns" get secret credit-authority-caller -o jsonpath='{.data.CREDITS_CALLER_ORG_ID}' | base64 -d)
    if [[ -z "$app" ]]; then
        hard_fail "X4 no application owns the Credit Authority's declaration, so nobody is admitted"
    elif ! ensure_spoke_kubeconfig; then
        hard_fail "X4 no spoke kubeconfig: the declaring application's service identity cannot be read"
    else
        declared=$(kc_spoke -n "tenant-$tenant-$app" get secret "$app-service-identity" -o jsonpath='{.data.OIDC_SERVICE_CLIENT_ID}' | base64 -d)
        if [[ -n "$admitted" && "$admitted" == "$declared" && -n "$admitted_org" && "$admitted_org" == "$declared_org" ]]; then
            pass "X4 the admitted caller is $app's service client, of $app's organisation"
        else
            hard_fail "X4 the admitted caller '${admitted}' (org '${admitted_org}') is not $app's service client '${declared}' (org '${declared_org}')"
        fi
    fi

    # Cluster-wide grants, asked of the API server by impersonation (a
    # SubjectAccessReview; nothing is changed).
    local sa can escalated=()
    while read -r sa; do
        [[ -n "$sa" ]] || continue
        [[ " ${CREDITS_EXEC_ALLOWED[*]} " == *" $sa "* ]] && continue
        for verb in "create pods/exec" "create pods/portforward"; do
            can=$(kc auth can-i $verb -n "$ns" --as="$sa")
            [[ "$can" == "yes" ]] && escalated+=("$sa: $verb")
        done
    done < <(kc get sa -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' \
                 | awk '$1 != "kube-system" {print "system:serviceaccount:" $1 ":" $2}')
    if (( ${#escalated[@]} == 0 )); then
        pass "X5 no service account but the platform's controllers can exec into or port-forward to it"
    else
        hard_fail "X5 service accounts that reach the ledger past the gateway: ${escalated[*]}"
    fi

    # Bypass attempts on the hub, from a running pod of another platform
    # namespace with a Node runtime (Infisical): the pod's address directly.
    local ip probe ledger_port gateway_port host public
    ip=$(kc -n "$ns" get pods -l app.kubernetes.io/name=credit-authority -o jsonpath='{.items[0].status.podIP}')
    probe='const n=require("net");const s=n.connect({host:process.argv[1],port:+process.argv[2]});s.on("connect",()=>{console.log("open");process.exit(0)});s.on("error",()=>{console.log("closed");process.exit(0)});setTimeout(()=>{console.log("timeout");process.exit(0)},4000)'
    local control
    ledger_port=$(kc -n platform-security exec deploy/infisical-standalone-infisical -- node -e "$probe" "$ip" 3068)
    gateway_port=$(kc -n platform-security exec deploy/infisical-standalone-infisical -- node -e "$probe" "$ip" 3000)
    # The control: a connection this pod is allowed to make. Without it, a
    # refusal could be this pod's own egress policy rather than the Credit
    # Authority's ingress, and the check would pass proving nothing.
    # Its OWN database, which infisical-db-access admits platform-security to on 5432.
    # Not another application's cluster: after ADR-112 each one admits only its own
    # consumer, so a probe at any other would be refused by THAT policy and the
    # control would prove the opposite of what it is for.
    control=$(kc -n platform-security exec deploy/infisical-standalone-infisical -- node -e "$probe" infisical-db-rw.platform-data.svc.cluster.local 5432)
    if [[ -z "$ip" || -z "$ledger_port" || -z "$gateway_port" ]]; then
        hard_fail "X6 cannot probe the pod from platform-security (address '${ip}')"
    elif [[ "$control" != "open" ]]; then
        hard_fail "X6 inconclusive: platform-security's own connection to infisical-db is '${control}', so a refusal proves nothing"
    elif [[ "$ledger_port" != "open" && "$gateway_port" != "open" ]]; then
        pass "X6 from another hub namespace, the ledger's port is $ledger_port and the gateway's is $gateway_port: only the Gateway reaches it"
    else
        hard_fail "X6 from platform-security the ledger port is '${ledger_port}' and the gateway port '${gateway_port}'; both must refuse"
    fi

    # Through the Gateway, from the declaring application's SDK on its spoke.
    host=$(kc -n "$ns" get httproute credit-authority -o jsonpath='{.spec.hostnames[0]}')
    if [[ -n "$app" && -n "$host" ]]; then
        # The SDK by its label: a workload is a Rollout or a Deployment, and the
        # pod is what both run.
        local sdk_pod
        sdk_pod=$(kc_spoke -n "tenant-$tenant-$app" get pods -l app=sdk -o jsonpath='{.items[0].metadata.name}')
        public=$(kc_spoke -n "tenant-$tenant-$app" exec "$sdk_pod" -- node -e \
            'fetch(process.argv[1]).then(r=>console.log(r.status)).catch(e=>console.log("error "+e.message))' "https://$host/health")
        if [[ "$public" == "200" ]]; then
            pass "X6 from $app's SDK, https://$host answers through the Gateway"
        else
            hard_fail "X6 from $app's SDK, https://$host/health answered '${public}'"
        fi
    fi

    local missing=() v
    for v in CREDITS_TOKEN CREDITS_ID_TOKEN CREDITS_OTHER_APP_TOKEN CREDITS_OTHER_TENANT_TOKEN CREDITS_EXPIRED_TOKEN; do
        [[ -n "${!v:-}" ]] || missing+=("$v")
    done
    if (( ${#missing[@]} > 0 )); then
        hard_fail "X7 not proven -- ${missing[*]} not set, so the suites did not run in full"
        return 0
    fi
    local ledger record log verdict
    ledger=$(kc -n "$ns" get configmap credit-authority -o jsonpath='{.data.ledger}')
    if [[ -z "$ledger" || -z "$host" ]]; then
        hard_fail "X7 the deployed Credit Authority names no ledger or host"
        return 0
    fi
    record="$VALIDATE_ROOT/docs/evidence/109-credit-authority/$(date -u +%Y%m%dT%H%M%SZ).md"
    log=$(mktemp)
    mkdir -p "$(dirname "$record")"
    if (cd "$VALIDATE_ROOT/test/e2e" && CREDITS_URL="https://$host" CREDITS_LEDGER="$ledger" \
            go test -count=1 -v ./credit-authority >"$log" 2>&1) && ! grep -q -- "--- SKIP" "$log"; then
        verdict=PASS
        pass "X7 the guarantees and isolation suites pass through https://$host -- evidence: $record"
    else
        verdict=FAIL
        hard_fail "X7 the guarantees or isolation suite failed or skipped -- evidence: $record"
    fi
    {
        echo "# ADR-109 activation evidence"
        echo
        echo "- X7: **$verdict**"
        echo "- At: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "- Hub: $(kubectl --kubeconfig="$HUB_KUBECONFIG" config current-context)"
        echo "- Endpoint: https://$host"
        echo "- Ledger: $ledger"
        echo "- Admitted caller: $admitted ($app, organisation $admitted_org)"
        echo "- Images:"
        kc -n "$ns" get pods -l app.kubernetes.io/name=credit-authority -o jsonpath='{range .items[0].status.initContainerStatuses[*]}  - {.name}: {.imageID}{"\n"}{end}{range .items[0].status.containerStatuses[*]}  - {.name}: {.imageID}{"\n"}{end}'
        echo
        echo '```'
        grep -E -- "^(=== RUN|--- (PASS|FAIL|SKIP)|PASS|FAIL|ok|    )" "$log"
        echo '```'
    } >"$record"
    rm -f "$log"
}
