import type { components } from "../api/generated/schema";
type Schemas = components["schemas"];
/** Egress profile of a template (api/v1alpha1 NetworkProfile). */
export type NetworkProfile = NonNullable<Schemas["TemplateView"]["networkProfile"]>;
export type TemplateView = Schemas["TemplateView"];
export type RuntimeKind = Schemas["RuntimeKind"];
export type ExperienceKind = Schemas["ExperienceKind"];
export type DataPolicy = Schemas["DataPolicy"];
