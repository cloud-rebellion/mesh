# Purpose-specific knowledge authoring

Mesh has a versioned library of note templates and reusable blocks. Writers retrieve
the definition they need, supply substantive content, and use the shared validator
and durable publisher. The thirteen base templates are approved. **Creating notes
from any registered template requires no template approval.** Adding or changing a
library definition requires the user's approval; an AI proposal never activates itself.

Each note begins with a useful Summary. Its title, ID, template/version, timestamps,
access scopes and provenance are metadata. The summary and explanatory prose live
once in the Markdown body. The search index derives compact cards and section
addresses from that body. A structural validation result is not factual verification.

## Retrieve a definition, then write

1. Call `mesh_templates` for the compact catalog.
2. Call `mesh_note_template` with `template` and optional `version` (currently 1).
   Its response contains the section keys, guidance and the common authoring schema.
3. Fetch only needed blocks with `mesh_block_template`. A code block requires its
   purpose, language, code, source/revision, explanation, verification, limitations
   and whether it is illustrative. Evidence and verification blocks keep their
   observation context and uncertainty with the claim.
4. Call `mesh_author_note` with `action: "prepare"` and `note: { ... }` to preview.
   Preview does not save or reserve the proposed identity. Known metadata is filled;
   missing content is shown explicitly. Never manufacture evidence, causes or owners.
5. Fill the selected sections, then use `action: "validate"`. A complete note can
   be saved with `action: "publish"`; unfinished material uses `action: "draft"`.
   `mesh_append_note` and `mesh_write_entity` also use the same publisher and format.

`template` selects a purpose, while `type` is the existing semantic category. When
type is omitted it is derived from the template. A conflicting type is rejected.

| Template | Type | Core content after Summary |
|---|---|---|
| `entity` | entity | Purpose; current state; interfaces; entry points |
| `method` | concept | Applicability; approach; variations; verification; limitations |
| `procedure` | concept | Prerequisites; steps; expected result; verification; recovery |
| `post-mortem` | post-mortem | Events; impact; timeline; supported causes; resolution; follow-ups |
| `decision` | decision | Context; options; decision; rationale; consequences |
| `finding` | note | Question; findings; evidence; uncertainty; next steps |
| `plan` | note | Objective/audience; context; approach; milestones; measures; dependencies |
| `troubleshooting` | gotcha | Symptoms; diagnosis; cause/hypothesis; remedy; verification |
| `concept` | concept | Definition; explanation; applicability; examples and limits |
| `reference` | concept | Scope; entries; interpretation; sources |
| `review` | note | Criteria; assessment; evidence; gaps; follow-ups |
| `status` | status | Current state; changes; verification; blockers; next steps |
| `index` | map | Scope; start here; grouped annotated links |

Optional blocks are `code`, `evidence`, `verification`, `timeline`, `comparison`,
`diagram`, `worked-example`, `checklist`, `recovery`, `follow-up`, and `table`. Only selected
blocks appear. Unknown versions, unknown fields and duplicate block IDs are rejected.
Use the exact keys returned by the library rather than guessing a heading name.
Headings are fixed; content inside them can use paragraphs, unordered lists, ordered
lists, tables and other ordinary Markdown. These formatting choices need no approval.
Use a table block when its separate provenance and retrieval address are useful.
Checklist, recovery and follow-up blocks include completion criteria. Record an
owner when known; an omitted owner does not imply an assignment.

Collections contain existing note IDs and Topics use `tags`. `related` and
`supersedes` are explicit, supported relationships. Membership is independent of
access scope. A link cannot expose a target to an audience that cannot read it.
MCP checks the current source file as well as indexed permissions. No speculative
semantic match is silently turned into an asserted relationship.

## Maintain existing documents

When a schema, specification, method or product state changes, maintain its existing
note. Routine edits based on registered templates need no template approval. Read the
current note and its relevant links first; preserve still-applicable evidence, caveats
and references. A new decision can have a separate `decision` note recording context,
options, rationale and consequences. Link that actual accessible note from the updated
document, and update the product index when its navigation needs to change. Use existing
`related`, `supersedes`, collections and wikilinks; do not invent relationship types.
For a changed shared method, inspect its connected consumers and record verified impact
and follow-ups. A link identifies a possible dependency, not proof that code needs a change.

1. Call `mesh_prepare_update` with `id`. It returns the complete editable `note` object,
   its `update_id` and exact `update_revision`, with sections, selected blocks, tags,
   collections and links prefilled. This is a writer workflow; readers use cards and
   bounded section fetches. Full preparation is bounded and never silently clipped.
2. Edit that object, adding supported links and retaining relevant historical substance.
3. Use `mesh_author_note` with `action: "validate"`, then `action: "publish"`.
   Both use the same object and shared publisher. A stale revision requires a fresh read
   and reconciliation; it cannot overwrite another editor's change.

Published-note updates retain the canonical file, stable ID, event/creation dates,
template/version, access scopes, original author/source and custom metadata. Mesh sets
`updated`, `updated_by` and `updated_agent` separately. Scope changes and template
conversion require their own controlled workflows. Legacy or manually extended bodies
that cannot be represented losslessly are refused until reconciled in a reviewed draft.
An incomplete edit cannot replace a published note; create a separate linked draft.

Local Mesh writers serialize each note and archive its exact previous bytes at
`.mesh/note-history/<id>/<revision>.md` before replacement. This private history is
excluded from ordinary indexing and sync; include it explicitly in backups. Hosted
updates retain original bytes in Git history and commit the replacement under the same
repository lock as team sync. Publication rechecks the client's current role, scope
permissions and folder-ACL closure under that lock. External filesystem editors must
coordinate with local publication; optimistic checks cannot serialize arbitrary editors.

Historical verification blocks remain evidence of the checks they describe.
`verified_at` is not prefilled on an update and is cleared unless the writer explicitly
records verification for the edited content. Editing never claims a fresh factual check.

The CLI uses the same system:

```sh
mesh update NOTE_ID --vault VAULT > edit.json
# Review and edit the complete JSON object, preserving its update_id/revision.
mesh update NOTE_ID --vault VAULT --spec edit.json --validate
mesh update NOTE_ID --vault VAULT --spec edit.json --by EDITOR
```

## Drafts and reading

`mesh_drafts` returns a bounded, scoped inbox with current revisions. Drafts can be
fetched explicitly; ordinary search excludes them, and draft supersession claims
cannot retire published guidance. To update or publish a saved draft, supply its
`draft_id` and exact `draft_revision` with the completed authoring object. Its
canonical file and ID remain stable, as do creation time and original provenance.
The inbox is a status-filtered view: publication removes it from that view without
breaking path links. Stale revisions are refused. Hosted publication uses the same
repository transaction lock as team sync. Hosted MCP authoring rejects folder ACL
configurations it cannot safely enforce. Web pending promotion refuses configured
folder rules, including admin requests, because per-author access alone cannot
establish the destination audience. It permits identified members when the provider
confirms no folder rules exist. An unrestricted shared-token callback cannot prove
that absence and remains refused. Local Mesh writers serialize draft updates
and check revisions immediately before publication. External editors must coordinate
with publication: the local guard is optimistic, not a filesystem-wide transaction.

`updated` records editing. `verified_at` is never inferred from formatting and
requires an explicit verification block with checks, context, results and gaps.
An agent remains responsible for making those statements truthful. Unknown causes
and unperformed checks must be described as such.

Cards provide a concise summary and stable section addresses. `mesh_fetch` accepts
a heading anchor or a stable section key; blocks have `block-<id>` anchors. Use
`block-<id>-<field-key>` for a specific block field, such as
`block-example-verification`. This keeps method verification distinct from code
verification. Cards list selected blocks alongside core sections and mark
`SectionsTruncated` when the bounded address list omits further entries. Prefer
`mesh_fetch_many` with an explicit budget for bounded detail. Fetching a child of
a code/evidence block carries the block's relevant provenance and limitations.
Truncation and incomplete-context warnings remain visible. Imported content retains
its untrusted-data markers. Access checks precede returning either content or anchors.

## Library changes

`mesh_propose_template` saves a **draft proposal** containing a rationale, proposed
definition and worked example. Registration is a reviewed source change to the
versioned registry, accompanied by examples and validation tests. The user approves
the definition before it is registered and released. Proposals alone cannot alter
the registry. This implementation deliberately has no agent-callable self-approval
or runtime activation endpoint. Once registered, a template is used through the
same ordinary note workflow without repeated approval.

Existing versions remain addressable. A definition change introduces a new version;
it does not silently reinterpret already-authored notes.

## Reviewed historical migration

The active writer no longer accepts `do`, `dont` or `why`. A legacy adapter still
reads historical fields under their original meaning and retains their links and
caveats. Their absence is not a modern completeness failure. Existing extraction
queue entries remain recoverable through the pending adapter.

Migrate explicit batches, beginning with current product entry points, frequently
used guidance, shared methods and operational incidents. Prepare substantive sections
from source evidence; a prohibition is not automatically impact and a rationale is
not automatically root cause. The migration preview preserves unsupported material
as historical evidence and flags missing structure. It never supplies invented facts.

```sh
mesh templates migration-preview --vault VAULT --requests requests.json --out preview.json
mesh templates migration-apply --vault VAULT --preview preview.json \
  --preview-hash HASH_FROM_PREVIEW --reviewed-ids NOTE_ID_1,NOTE_ID_2
```

The request file is an array of `{ "path": "relative/file.md", "id": "stable-id",
"spec": { ... } }`. The spec uses the shared `NewNoteSpec` JSON fields as shown in
the preview command's help/examples. Review the exact rendered candidates, caveats,
references and source hashes before applying the selected IDs. Drift invalidates
the preview. Original bytes and receipts are retained under `.mesh/migrations/`.
Conversion preserves identity, creation/event dates, provenance, access scopes and
historical verification; it does not certify that old guidance is current.
The old `mesh structure --fill-bodies` operation is retired. It refuses both preview
and apply because shorthand cannot establish incident causes, impact or conclusions.
Use a reconstructed purpose-specific candidate in a reviewed migration preview.

## Rollout and acceptance

Deploy with `MESH_AUTHORING_MODE=readers-only`, upgrade all readers, and verify legacy
and versioned notes remain readable. This mode permits catalogs, preparation,
validation and migration previews while refusing durable template publication and
migration application. Set `MESH_AUTHORING_MODE=enabled` after reader verification,
then enable the new authoring clients. Unset mode preserves ordinary local authoring;
unknown values refuse publication. Writers and extraction prompts must be upgraded
together because retired field arguments are rejected. The pending-store migration
is additive and preserves historical queue content. Keep a backup before upgrading;
an older binary must not be pointed at a newer unsupported index schema.

Validate against the engineering incident, marketing strategy and sales procedure
journeys in the MCP acceptance tests. Also verify draft completion, stale revisions,
bounded code/evidence retrieval, current-file scopes, hosted commit/sync, and reviewed
migration drift/retention. Review the examples for explanatory usefulness separately
from mechanical checks. Local tests establish local behavior; release, installation,
hosted activation and historical batch migration are separate rollout steps.
