package controller

// The Credit Authority is a selectable platform capability of the box
// (ADR-109), run on the hub like the box's other platform services and turned
// on by capabilities.credits.enabled. It is a platform capability with one
// caller (platform_capability.go); this is what is its own.
//
// Its database, role and credential are platform prerequisites present on
// every box (credits-db, HubEnvironment, the application secret mappings).
var creditAuthority = platformCapability{
	name:              "credits",
	title:             "the Credit Authority",
	namespace:         "platform-credits",
	marker:            "credit-authority",
	projectIDKey:      "credits-oidc-project-id",
	callerClientIDKey: "credits-caller-client-id",
	callerOrgIDKey:    "credits-caller-org-id",
	ownerCondition:    "CreditAuthorityOwner",
	controllerName:    "creditauthority",
}
