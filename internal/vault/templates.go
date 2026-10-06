// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import "fmt"

// SectionSpec describes an authored body section, never a frontmatter prose field.
type SectionSpec struct {
	Key      string `json:"key"`
	Heading  string `json:"heading"`
	Guidance string `json:"guidance"`
	Required bool   `json:"required"`
}

// NoteTemplate is a versioned authoring contract. Type carries the established
// note lifecycle; ID distinguishes purpose-specific layouts within that type.
type NoteTemplate struct {
	ID       string        `json:"id"`
	Version  int           `json:"version"`
	Type     NoteType      `json:"type"`
	Purpose  string        `json:"purpose"`
	Sections []SectionSpec `json:"sections"`
}

type FieldSpec struct {
	Key      string `json:"key"`
	Heading  string `json:"heading"`
	Guidance string `json:"guidance"`
	Required bool   `json:"required"`
}

type BlockTemplate struct {
	ID      string      `json:"id"`
	Version int         `json:"version"`
	Purpose string      `json:"purpose"`
	Fields  []FieldSpec `json:"fields"`
}

// BlockSpec contains author input; only identity/version metadata enters YAML.
type BlockSpec struct {
	Template string            `json:"template"`
	Version  int               `json:"version,omitempty"`
	ID       string            `json:"id"`
	Fields   map[string]string `json:"fields"`
}

type BlockMetadata struct {
	Template string `yaml:"template" json:"template"`
	Version  int    `yaml:"version" json:"version"`
	ID       string `yaml:"id" json:"id"`
}

func section(key, heading, guidance string) SectionSpec {
	return SectionSpec{Key: key, Heading: heading, Guidance: guidance, Required: true}
}

func field(key, heading, guidance string) FieldSpec {
	return FieldSpec{Key: key, Heading: heading, Guidance: guidance, Required: true}
}

func optionalField(key, heading, guidance string) FieldSpec {
	return FieldSpec{Key: key, Heading: heading, Guidance: guidance}
}

var noteTemplates = []NoteTemplate{
	{"entity", 1, TypeEntity, "A concise current overview of a product, repository, service, person or other entity.", []SectionSpec{
		section("purpose", "Purpose", "Explain what this entity is for and who it serves."),
		section("current_state", "Current state", "State the current situation, its date and what remains unverified."),
		section("interfaces", "Responsibilities and interfaces", "Describe boundaries, responsibilities and relevant interactions."),
		section("entry_points", "Important entry points", "Give useful existing links or paths with a short explanation; never fabricate targets."),
	}},
	{"method", 1, TypeConcept, "A reusable approach that can be adapted to different situations.", []SectionSpec{
		section("applicability", "Problem and applicability", "Describe the problem, when this approach fits and when it does not."),
		section("approach", "Approach", "Explain the method and why its steps help."),
		section("variations", "Variations", "Describe adaptations; explicitly say when none are known."),
		section("verification", "Verification", "Explain how effectiveness is checked and what has actually been checked."),
		section("limitations", "Limitations", "Record assumptions, uncertainty and known limits."),
	}},
	{"procedure", 1, TypeConcept, "An actionable sequence with prerequisites, verification and recovery.", []SectionSpec{
		section("prerequisites", "Prerequisites", "State the required context, permissions, inputs and applicability."),
		section("steps", "Steps", "Give the ordered actions and meaningful decision points."),
		section("expected_result", "Expected result", "Describe the observable outcome."),
		section("verification", "Verification", "Separate proposed checks from checks actually performed."),
		section("recovery", "Failure and recovery", "Explain known failure signals and supported recovery; label unknowns."),
	}},
	{"post-mortem", 1, TypePostMortem, "A dated incident account with supported causes and follow-up actions.", []SectionSpec{
		section("what_happened", "What happened", "Describe the incident factually and bound the affected context."),
		section("impact", "Impact", "State who or what was affected, duration and uncertainty."),
		section("timeline", "Key timeline", "Record significant dated events; do not invent missing timestamps."),
		section("root_cause", "Root cause and contributing factors", "Distinguish supported causes, contributing factors and hypotheses; unknown is valid."),
		section("resolution", "Resolution and verification", "Describe the response and the evidence for recovery, including remaining gaps."),
		section("follow_up", "Follow-ups", "List actions with completion criteria; name owners and dates only when known."),
	}},
	{"decision", 1, TypeDecision, "A choice, its alternatives, rationale and accepted consequences.", []SectionSpec{
		section("context", "Context", "Explain the situation, constraints and decision scope."),
		section("options", "Options considered", "Record actual alternatives and relevant comparisons."),
		section("decision", "Decision", "State the selected option and its status plainly."),
		section("rationale", "Rationale", "Explain the reasons, evidence and uncertainty."),
		section("consequences", "Consequences and review triggers", "Describe trade-offs, follow-on effects and conditions that warrant review."),
	}},
	{"finding", 1, TypeNote, "An observation or research result, grounded in evidence and uncertainty.", []SectionSpec{
		section("question", "Question", "State the question and the investigated scope."),
		section("findings", "Findings", "Separate observations from interpretations and hypotheses."),
		section("evidence", "Evidence", "Identify sources, observation dates and the results they support."),
		section("limitations", "Limitations and uncertainty", "State coverage limits, conflicting evidence and open questions."),
		section("next_steps", "Implications and next steps", "Explain practical implications and unresolved follow-up work."),
	}},
	{"plan", 1, TypeNote, "An intended course of action with outcomes, milestones and risks.", []SectionSpec{
		section("objective", "Objective and audience", "State the intended outcome and who the plan serves."),
		section("context", "Context", "Describe the starting situation and constraints."),
		section("approach", "Approach", "Explain the proposed strategy and its reasoning."),
		section("milestones", "Milestones and actions", "List meaningful milestones and actions; dates and owners only when known."),
		section("success_measures", "Success measures", "Describe observable success criteria; do not claim future verification."),
		section("dependencies", "Dependencies and risks", "Record dependencies, assumptions, uncertainty and risks."),
	}},
	{"troubleshooting", 1, TypeGotcha, "A reusable problem pattern with diagnosis, remedy and verification.", []SectionSpec{
		section("symptoms", "Symptoms and conditions", "Describe the observable problem and applicable context."),
		section("diagnosis", "Diagnosis", "Give discriminating checks and their meaning."),
		section("cause", "Cause or hypothesis", "Distinguish confirmed causes from hypotheses and unknowns."),
		section("remedy", "Remedy", "Describe the supported remedy and its conditions."),
		section("verification", "Verification", "Explain how resolution is checked and what is already verified."),
	}},
	{"concept", 1, TypeConcept, "An explanation of an idea and where it applies.", []SectionSpec{
		section("definition", "Definition", "Define the idea plainly."),
		section("explanation", "Explanation", "Explain how it works and the relationships that matter."),
		section("applicability", "Applicability", "Describe when the idea is useful."),
		section("examples", "Examples and limitations", "Give grounded or explicitly illustrative examples and the limits of the idea."),
	}},
	{"reference", 1, TypeConcept, "A compact lookup reference with interpretation and sources.", []SectionSpec{
		section("scope", "Scope", "State what the reference covers and its version or date where relevant."),
		section("entries", "Structured entries", "Provide the lookup entries in a consistent useful structure."),
		section("interpretation", "Interpretation", "Explain how to use entries and their limitations."),
		section("sources", "Sources", "Identify the authoritative sources and relevant dates."),
	}},
	{"review", 1, TypeNote, "An assessment against explicit criteria, including evidence and gaps.", []SectionSpec{
		section("criteria", "Scope and criteria", "State what was reviewed and the assessment criteria."),
		section("assessment", "Assessment", "Give the assessment and distinguish findings from judgement."),
		section("evidence", "Evidence", "Cite the observations and sources supporting the assessment."),
		section("gaps", "Gaps", "State unverified areas, limitations and unresolved concerns."),
		section("follow_up", "Follow-ups", "List the next actions and completion criteria; owners only when known."),
	}},
	{"status", 1, TypeStatus, "A dated working-state snapshot with verification, blockers and next steps.", []SectionSpec{
		section("current_state", "Current state", "State the situation as of this snapshot."),
		section("changes", "Changes", "Describe changes since the relevant previous state; explicitly say when none are known."),
		section("verification", "Verification", "Record what was checked, the context and the remaining verification gaps."),
		section("blockers", "Blockers", "List actual blockers or explicitly state that none are known."),
		section("next_steps", "Next steps", "Give the remaining actions and review needs."),
	}},
	{"index", 1, TypeMap, "A stable collection entry point with grouped, annotated links.", []SectionSpec{
		section("scope", "Scope", "Explain the collection's purpose and inclusion boundaries."),
		section("start_here", "Start here", "Link the most useful existing entry points; explicitly state when the collection is not yet populated."),
		section("grouped_links", "Grouped annotated links", "Group existing note links and explain their usefulness. Generated indexes must declare their generation scope and freshness."),
	}},
}

// Product specification definitions were approved by the user on 2026-10-07.
// Preserve their versioned section contracts when revising note content.
func init() {
	noteTemplates = append(noteTemplates, []NoteTemplate{
		{"prd", 1, TypeNote, "Define the product problem, intended outcomes and observable requirements before choosing implementation details.", []SectionSpec{
			section("problem", "Problem and audience", "Describe the observed need, affected users and evidence. Label assumptions and distinguish requested behavior from current behavior."),
			section("outcomes", "Outcomes and success measures", "State desired outcomes and proposed measures. Use numerical targets only when agreed or sourced; distinguish targets from measured results."),
			section("scope", "Scope and boundaries", "Define the included capability and its boundaries. Refer to existing context instead of recreating the whole product overview."),
			section("requirements", "Product requirements", "Give each requirement a stable ID, an observable behavior and known priority. Preserve IDs during revision and link actual sources or decisions."),
			section("dependencies", "Dependencies and constraints", "Identify relevant policies, systems and constraints. Link the authoritative notes and mark unresolved dependencies honestly."),
			section("acceptance", "Acceptance criteria", "Associate each criterion with requirement IDs and a concrete expected outcome. State whether verification is planned or recorded."),
			section("open_questions", "Risks and open questions", "Record unresolved product questions, assumptions and risks. Name decision owners only when known."),
		}},
		{"trd", 1, TypeNote, "Translate linked product needs and flows into technical constraints, interfaces and verifiable engineering requirements.", []SectionSpec{
			section("context", "Scope and source requirements", "Identify the technical scope and link source product requirement IDs and relevant flows. Do not duplicate their prose."),
			section("architecture", "Architecture and interfaces", "Describe boundaries, components and contracts. Distinguish proposed designs, approved decisions and observed implementation; link reusable methods."),
			section("requirements", "Technical requirements", "Give requirements stable IDs and trace each to a product need, constraint or decision. Describe observable technical behavior."),
			section("quality", "Quality attributes and limits", "Cover relevant security, accessibility, performance, availability, privacy and observability requirements. Mark unagreed limits instead of inventing targets."),
			section("dependencies", "Dependencies and constraints", "Identify interfaces, platform constraints and known dependencies. Keep implementation choices provisional until supported by a decision."),
			section("verification", "Verification and acceptance", "Map checks to technical requirement IDs. Separate a proposed test plan from completed checks and retain applicable version and evidence when checked."),
			section("open_questions", "Risks and open questions", "Record technical uncertainty, trade-offs and unresolved decisions, including consequences for dependent specifications."),
		}},
		{"app-flow", 1, TypeConcept, "Explain user journeys, application states, transitions and recovery behavior with links to requirements and design.", []SectionSpec{
			section("actors", "Actors and entry points", "Identify who enters the flow, through which entry points and with which prerequisites. Record only supported roles and access rules."),
			section("main_flows", "Main flows", "Describe the main journey in ordered steps with stable flow or step IDs. Refer to requirement IDs and describe meaningful system responses."),
			section("states", "States and transitions", "Describe important states, transitions and their conditions. Use an existing diagram or table block when it improves retrieval; label proposed states."),
			section("alternatives", "Alternative and recovery flows", "Explain relevant errors, empty states, cancellation, permission failures and retries. State unknown recovery behavior explicitly."),
			section("interfaces", "Screens and integration references", "Link actual screens, design briefs and system interfaces. Identify missing designs instead of inventing approved screens or endpoints."),
			section("acceptance", "Acceptance and open questions", "Describe the flow's observable acceptance conditions and unresolved interactions. Separate walkthrough plans from recorded usability or execution evidence."),
		}},
		{"design-brief", 1, TypeNote, "Define the experience intent, design scope, constraints and review criteria for a product or feature.", []SectionSpec{
			section("objective", "Objective and audience", "State the experience problem, audience and desired outcome; link the source product requirements and flow."),
			section("experience", "Experience and design principles", "Describe concrete interaction, content and visual principles and why they fit the users. Avoid generic aesthetic slogans."),
			section("deliverables", "Deliverables and scope", "Identify the required design outputs and coverage. Keep deliverables proportionate to the feature rather than requiring every possible artifact."),
			section("constraints", "Accessibility and constraints", "Record relevant accessibility, platform, brand, localization and existing component constraints. Distinguish requirements from checked compliance."),
			section("references", "References and rationale", "Link relevant existing components, designs, research and decisions, explaining their relevance. Label illustrative references and missing evidence."),
			section("acceptance", "Review criteria and open questions", "State how the design will be reviewed against requirements and flows. Identify open design questions; recorded reviews need evidence and context."),
		}},
		{"backend-schema", 1, TypeConcept, "Describe the data model, integrity constraints, access boundaries and evolution of a backend without assuming a database engine.", []SectionSpec{
			section("scope", "Scope and lifecycle", "Define the modeled domain and whether it describes current, proposed or historical structure. Identify source/revision when known."),
			section("entities", "Entities and relationships", "Describe entities, identifiers, ownership and relationship cardinality. Use an existing diagram or table block where helpful; do not infer relationships from naming alone."),
			section("structure", "Fields, constraints and indexes", "Document important fields, keys, validation, uniqueness, referential integrity and indexes with their reasons. Distinguish proposed constraints from enforced ones."),
			section("access", "Access, tenancy and sensitive data", "Describe relevant access boundaries, tenant separation, sensitive fields and lifecycle obligations. Link the applicable policy and distinguish policy from implementation proof."),
			section("evolution", "Migrations and compatibility", "Describe the supported schema evolution and compatibility plan. Source-derived schema/code requires provenance; proposed examples must be labeled illustrative."),
			section("operations", "Integrity, retention and recovery", "Describe consistency, retention, deletion, backup and recovery requirements where relevant. Separate intended guarantees from tested operational evidence."),
			section("verification", "Verification and open questions", "Record planned or performed checks, applicable schema version and remaining modeling uncertainty. Formatting does not verify a schema."),
		}},
		{"implementation-plan", 1, TypeNote, "Turn linked requirements and design decisions into ordered development work, verification, rollout and recovery.", []SectionSpec{
			section("scope", "Scope and source requirements", "Link the product and technical requirements, flows, design and schema used by this plan. Identify what is ready and what still needs a decision."),
			section("starting_point", "Starting point and code entry points", "Describe the inspected starting state and actual relevant files, symbols or interfaces with revision when known. Do not fabricate repository paths."),
			section("work", "Work breakdown and dependencies", "Break work into bounded tasks with requirement references, dependencies and completion criteria. Supply owners and dates only when known."),
			section("sequence", "Implementation sequence", "Describe the ordered changes and compatibility dependencies, with relevant reusable methods and decisions. Mark blocked tasks and prerequisites."),
			section("verification", "Test and acceptance plan", "Map meaningful checks to task and requirement IDs; use code/evidence/verification blocks when useful. Separate planned tests from executed results."),
			section("rollout", "Rollout and recovery", "Describe the release sequence, compatibility, rollback/recovery and acceptance evidence. Separate proposed actions from authorized or completed deployment."),
			section("open_questions", "Risks and unresolved decisions", "State blockers, assumptions and decision dependencies that can change the plan. Link follow-ups and include ownership only when known."),
		}},
	}...)
}

var blockTemplates = []BlockTemplate{
	{"table", 1, "A fetchable Markdown table with attributable or explicitly illustrative entries.", []FieldSpec{
		field("purpose", "Purpose", "Explain what this table helps the reader compare or find."),
		field("table", "Table", "Provide the Markdown table verbatim; never invent numbers or missing values."),
		field("source", "Source", "Identify the data source and context, or explicitly label the table illustrative."),
		field("interpretation", "Interpretation", "Explain how to read the table without overstating what it establishes."),
		field("limitations", "Limitations", "State missing entries, uncertainty and relevant scope limits."),
	}},
	{"code", 1, "A contextual code snippet, with source/version, verification and limits.", []FieldSpec{
		field("purpose", "Purpose", "State what this snippet demonstrates or solves."),
		field("language", "Language", "Name the code language."),
		field("code", "Code", "Provide the code without altering it or embedding an outer fence."),
		field("source", "Source", "Identify the source, or explicitly state that the code is illustrative."),
		field("revision", "Revision", "Identify the applicable revision or explicitly state that it is unknown."),
		field("explanation", "Explanation", "Explain the relevant behavior and context."),
		field("verification", "Verification", "State checks and results, or explicitly mark the snippet unverified."),
		field("limitations", "Limitations", "State assumptions and limits."),
		field("illustrative", "Illustrative", "Use true for an illustrative example, false for source-derived code."),
	}},
	{"evidence", 1, "An attributable observation supporting a specific claim.", []FieldSpec{
		field("claim", "Claim", "State the precise claim this evidence supports."),
		field("source", "Source", "Identify the source or observation."),
		field("observed_at", "Observation date", "Record the observation date or explicitly state unknown."),
		field("result", "Result", "Describe the actual observed result."),
		field("uncertainty", "Uncertainty", "State limits, ambiguity or conflicting evidence."),
	}},
	{"verification", 1, "Checks, their execution context, results and gaps.", []FieldSpec{
		field("checks", "Checks", "Describe the checks actually performed or explicitly proposed."),
		field("context", "Version or context", "Identify the checked version, environment or other relevant context."),
		field("results", "Results", "State the observed results or explicitly mark unperformed checks."),
		field("gaps", "Gaps", "Describe what the checks do not establish."),
	}},
	{"timeline", 1, "A detailed sequence supporting a shorter account.", []FieldSpec{
		field("scope", "Scope", "Explain the event and time boundaries."),
		field("events", "Events", "List dated events and sources; mark unknown dates."),
		field("uncertainty", "Uncertainty", "Record missing events, clock differences and uncertain ordering."),
	}},
	{"comparison", 1, "A comparison against explicit dimensions.", []FieldSpec{
		field("options", "Options", "Identify the actual alternatives."),
		field("criteria", "Criteria", "Name the relevant comparison dimensions."),
		field("comparison", "Comparison", "Provide the comparison and distinguish evidence from judgement."),
		field("limitations", "Limitations", "State missing evidence and limits."),
	}},
	{"diagram", 1, "A contextual diagram with an explanation and limits.", []FieldSpec{
		field("purpose", "Purpose", "State what the diagram explains."),
		field("diagram", "Diagram", "Provide Mermaid or a source link; label illustrative content."),
		field("explanation", "Explanation", "Explain the important relationships."),
		field("limitations", "Limitations", "State scope, omissions and uncertainty."),
	}},
	{"worked-example", 1, "An explicitly scoped example of applying knowledge.", []FieldSpec{
		field("context", "Context", "State the example context and whether it is illustrative."),
		field("inputs", "Inputs", "Give the relevant inputs."),
		field("walkthrough", "Walkthrough", "Show the application step by step."),
		field("result", "Result", "State the observed or illustrative outcome."),
		field("limitations", "Limitations", "Describe how far the example generalizes."),
	}},
	{"checklist", 1, "A bounded set of review or execution checks.", []FieldSpec{
		field("purpose", "Purpose", "Explain when this checklist applies."),
		field("items", "Items", "List actionable checks without claiming they have been completed."),
		field("completion", "Completion criteria", "Describe what constitutes completion."),
		optionalField("owner", "Owner", "Name an owner only when known."),
	}},
	{"recovery", 1, "A supported failure response with verification and limits.", []FieldSpec{
		field("trigger", "Trigger", "State the observed condition that warrants recovery."),
		field("prerequisites", "Prerequisites", "State context, permissions and prerequisites."),
		field("steps", "Steps", "Give the supported recovery sequence."),
		field("verification", "Verification", "Describe observable recovery checks and actual evidence."),
		field("completion", "Completion criteria", "State the observable conditions for ending recovery; do not claim completion without evidence."),
		optionalField("owner", "Owner", "Name an owner only when known."),
		field("limitations", "Limitations", "State unsupported cases and residual uncertainty."),
	}},
	{"follow-up", 1, "A concrete follow-up action with completion criteria.", []FieldSpec{
		field("action", "Action", "State the action and its reason."),
		field("completion", "Completion criteria", "Describe the observable completion condition."),
		optionalField("owner", "Owner", "Name an owner only when known."),
		optionalField("due", "Due", "Record a due date only when agreed."),
		optionalField("status", "Status", "Record the actual action state."),
	}},
}

// Templates returns independent copies so callers cannot mutate the shared contract.
func Templates() []NoteTemplate {
	out := make([]NoteTemplate, len(noteTemplates))
	for i, t := range noteTemplates {
		t.Sections = append([]SectionSpec(nil), t.Sections...)
		out[i] = t
	}
	return out
}

// TemplateFor treats omitted version (0) as current v1; unknown versions fail closed.
func TemplateFor(id string, version int) (NoteTemplate, error) {
	if version == 0 {
		version = 1
	}
	for _, t := range Templates() {
		if t.ID == id && t.Version == version {
			return t, nil
		}
	}
	return NoteTemplate{}, fmt.Errorf("%w: unknown template %q version %d", ErrInvalidSpec, id, version)
}

func BlockTemplates() []BlockTemplate {
	out := make([]BlockTemplate, len(blockTemplates))
	for i, t := range blockTemplates {
		t.Fields = append([]FieldSpec(nil), t.Fields...)
		out[i] = t
	}
	return out
}

func BlockTemplateFor(id string, version int) (BlockTemplate, error) {
	if version == 0 {
		version = 1
	}
	for _, t := range BlockTemplates() {
		if t.ID == id && t.Version == version {
			return t, nil
		}
	}
	return BlockTemplate{}, fmt.Errorf("%w: unknown block template %q version %d", ErrInvalidSpec, id, version)
}

func DefaultTemplate(t NoteType) string {
	switch t {
	case TypeEntity:
		return "entity"
	case TypeDecision:
		return "decision"
	case TypePostMortem:
		return "post-mortem"
	case TypeGotcha:
		return "troubleshooting"
	case TypeConcept:
		return "concept"
	case TypeMap:
		return "index"
	case TypeStatus:
		return "status"
	default:
		return "finding"
	}
}
