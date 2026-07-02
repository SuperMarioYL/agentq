package protocol

import _ "embed"

// ApprovalEnvelopeSchema is the JSON Schema (draft 2020-12) for the
// ApprovalEnvelope wire format, embedded from docs/approval-envelope.schema.json
// so the running daemon can serve the exact same contract that ships in the repo.
//
// Publishing this schema is the point of the protocol moat: a third-party agent
// runtime can fetch it (GET /schema/approval-envelope.json), validate its own
// output against it, and POST conforming envelopes to POST /api/envelopes — no
// agentq wrapper or stdio scraping required. The schema and the Go structs in
// approval.go are kept in lockstep; TestApprovalEnvelopeSchemaMatchesStruct in
// schema_test.go fails the build if they drift.
//
//go:embed schema/approval-envelope.schema.json
var ApprovalEnvelopeSchema []byte

// ApprovalEnvelopeSchemaContentType is the MIME type the daemon serves the
// schema with. application/schema+json is the registered type for JSON Schema
// documents.
const ApprovalEnvelopeSchemaContentType = "application/schema+json"
