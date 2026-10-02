import type { components } from "../api/generated/schema";
type Schemas = components["schemas"];
/** Egress profile of a template (api/v1alpha1 NetworkProfile). */
export type NetworkProfile = NonNullable<Schemas["TemplateView"]["networkProfile"]>;
// `family` (the catalog name shared by every revision) is a contract addition
// typed here until the generated schema carries it.
export type TemplateView = Schemas["TemplateView"] & { family?: string };
export type RuntimeKind = Schemas["RuntimeKind"];
export type ExperienceKind = Schemas["ExperienceKind"];
export type DataPolicy = Schemas["DataPolicy"];
