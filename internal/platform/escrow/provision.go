package escrow

// pathProjects creates a project.
//
// /projects, not /workspace. routes/v1/index.ts registers TWO project routers:
// registerDeprecatedProjectRouter under prefix "/workspace", and
// registerProjectRouter -- the one serving POST / with a ProjectType body --
// under prefix "/projects". Reading project-router.ts alone shows `url: "/"` and
// says nothing about where it is mounted, and the deprecated path is the one
// that looks familiar, so the first attempt used /api/v1/workspace and got
//
//	404 Route POST:/api/v1/workspace not found
//
// The handler accepts AuthMode.IDENTITY_ACCESS_TOKEN and takes projectName with
// a ProjectType defaulting to SecretManager.
const pathProjects = "/api/v1/projects"

// projectTypeSecretManager is the only type an escrow can use.
//
// backend/src/db/schemas/models.ts: SecretManager = "secret-manager". A project
// of any other type authenticates identically and then refuses every read and
// write with "The project is of type kms. Operations of type secret-manager are
// not allowed." The type is fixed at creation and cannot be changed afterwards,
// which is why naming it here matters: the alternative is discovering it from a
// 400 inside a reconcile, which is how a KMS project went on being configured as
// this platform's escrow without anything ever being escrowed.
const projectTypeSecretManager = "secret-manager"

// The escrow project is created by the signed-in administrator, in
// provision_identity.go.
//
// The machine-identity path that used to live here is gone, along with the
// --owner-email invitation it needed. It authenticated AS the machine identity,
// which meant the operator had to create one in Infisical's dashboard before the
// CLI could act -- and because Infisical adds only the CREATING actor to a
// project, the project then had no human members and did not appear in anyone's
// project list. The invitation existed to repair that.
//
// Creating the project as the human removes the cause rather than the symptom:
// they are its admin because they made it, and the automation identity is given
// the project role it actually needs instead of inheriting admin by accident.
