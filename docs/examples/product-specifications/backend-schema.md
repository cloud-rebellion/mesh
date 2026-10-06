# Illustrative review inbox: backend/database schema

## Summary

Fictional review-inbox example demonstrating this proposed layout. It records proposed behavior and honest gaps, not production facts or verification.

## Scope and lifecycle

Illustrative proposed model for review items and completion. It is not an extracted or deployed schema; no source revision is available.

## Entities and relationships

Proposed entities: ReviewItem and Reviewer. Each ReviewItem has an assigned reviewer in this example. The real assignment model remains a product decision.

## Fields, constraints and indexes

| Proposed field | Meaning | Proposed constraint |
|---|---|---|
| id | Review item identity | Unique and required |
| reviewer_id | Assigned reviewer | Required reference |
| status | Review state | Pending or complete |
| completed_at | Completion time | Present only when complete |
An index supporting reviewer and status lookup is proposed; no DDL or enforcement is claimed.

## Access, tenancy and sensitive data

Access must support TR-001. Tenant rules, sensitive content classification and field-level policy are undecided. No implemented row or document access control is asserted.

## Migrations and compatibility

A migration would add the reviewed structure and validate existing data before enforcing constraints. Database-specific steps and application compatibility sequencing remain unplanned.

## Integrity, retention and recovery

Completion updates must preserve the state/time constraint and satisfy TR-002. Retention, backup and recovery policy are unknown; no restoration evidence is claimed.

## Verification and open questions

Planned checks: reject invalid state/time combinations, enforce references and refuse cross-reviewer access. No migration, query or recovery check has been performed.
