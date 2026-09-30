// Named aliases for the generated schema components, so app code can write
// `Candidate` instead of `components["schemas"]["Candidate"]`. Add a line
// here when a new schema lands in api/openapi.yaml; never declare a shape.
import type { components } from "./schema";

type Schemas = components["schemas"];

export type Persona = Schemas["Persona"];
export type HealthResponse = Schemas["HealthResponse"];
export type HealthStatus = Schemas["HealthStatus"];
export type ApiError = Schemas["Error"];

export type Candidate = Schemas["Candidate"];
export type CandidateInput = Schemas["CandidateInput"];
export type CandidateStatus = Schemas["CandidateStatus"];

export type Profile = Schemas["Profile"];
export type ProfileInput = Schemas["ProfileInput"];
export type Availability = Schemas["Availability"];

export type Role = Schemas["Role"];
export type RoleInput = Schemas["RoleInput"];
export type RoleStatus = Schemas["RoleStatus"];

export type Match = Schemas["Match"];
export type MatchCreate = Schemas["MatchCreate"];
export type MatchUpdate = Schemas["MatchUpdate"];
export type MatchStatus = Schemas["MatchStatus"];
export type ReleaseInput = Schemas["ReleaseInput"];
