package controller

import (
	"testing"

	client2 "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
)

// ADR-094 invariant 2: a target's allowlist names the caller by the client a
// receiver will actually see as `azp`.
//
// That is the caller's EXCHANGE client, because Zitadel stamps an exchanged
// token with the client that authenticated the exchange rather than the one
// whose token was the subject -- createExchangeAccessToken and createExchangeJWT
// both pass `client.client.ClientID` into CreateOIDCSession.
//
// These tests exist because the first implementation published the tenant's
// PUBLIC PKCE client id instead, and nothing failed: no test named the value, so
// the only symptom would have been every cross-application call refused on the
// first box that declared a dependency.

func TestTheAllowlistNamesTheExchangeClientNotTheBrowserClient(t *testing.T) {
	const (
		browserClientID  = "oranger-public-pkce@nutgraf"
		exchangeClientID = "oranger-gateway-exchange@nutgraf"
	)

	// What a mint returns: the fleet's own declared clients, plus the exchange
	// client the platform appends.
	clients := []client2.DeclaredClient{
		{Name: "bff", ClientID: "oranger-bff@nutgraf", ClientSecret: "x"},
		{Name: gatewayExchangeClientName("oranger"), ClientID: exchangeClientID, ClientSecret: "y"},
	}

	got := exchangeClientIDFrom(clients, gatewayExchangeClientName("oranger"))
	if got != exchangeClientID {
		t.Fatalf("allowlist would name %q; the receiver sees %q as azp", got, exchangeClientID)
	}
	if got == browserClientID {
		t.Fatal("named the browser client: any token from the caller's own login would satisfy the target")
	}
}

func TestAnotherApplicationsExchangeClientIsNotMatched(t *testing.T) {
	// One tenant, two applications. Each has its own exchange client (ADR-088),
	// and resolving oranger's must never return waypoint's -- that would admit
	// the wrong application to the target.
	clients := []client2.DeclaredClient{
		{Name: gatewayExchangeClientName("waypoint"), ClientID: "waypoint-gateway-exchange@nutgraf"},
	}

	if got := exchangeClientIDFrom(clients, gatewayExchangeClientName("oranger")); got != "" {
		t.Fatalf("resolved %q for oranger from waypoint's client alone; want no match", got)
	}
}

func TestNoExchangeClientResolvesToEmptyRatherThanSomethingElse(t *testing.T) {
	// Empty is the contract, and the caller treats it as pending. Returning any
	// other client's id would leave a target that looks configured while
	// admitting a caller nobody authorised -- worse than admitting nobody, which
	// merely fails closed until the next reconcile.
	clients := []client2.DeclaredClient{
		{Name: "bff", ClientID: "oranger-bff@nutgraf"},
		{Name: "worker", ClientID: "oranger-worker@nutgraf"},
	}

	if got := exchangeClientIDFrom(clients, gatewayExchangeClientName("oranger")); got != "" {
		t.Fatalf("fell back to %q; want \"\" so the caller waits", got)
	}
	if got := exchangeClientIDFrom(nil, gatewayExchangeClientName("oranger")); got != "" {
		t.Fatalf("resolved %q from no clients at all", got)
	}
}

func TestAMintedClientWithNoIdYetDoesNotResolve(t *testing.T) {
	// The name matches but the issuer returned no id. Returning "" sends the
	// caller to its Infisical fallback, where the id published at mint time
	// lives; returning the name or a blank would publish a useless allowlist.
	clients := []client2.DeclaredClient{
		{Name: gatewayExchangeClientName("oranger"), ClientID: ""},
	}

	if got := exchangeClientIDFrom(clients, gatewayExchangeClientName("oranger")); got != "" {
		t.Fatalf("resolved %q from a client carrying no id", got)
	}
}
