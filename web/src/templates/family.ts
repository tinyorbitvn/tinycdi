// Template ids are per revision: a chart upgrade publishes a new object and
// removes the old one, so a workspace can pin an id the catalog no longer
// lists. The family (the catalog name) is stable across revisions.

interface FamilyRef {
  id: string;
  /** Catalog name shared by every revision (absent on servers that predate it). */
  family?: string;
}

/** Stable join key between workspaces and templates; the id is the fallback. */
export function templateFamily(ref: FamilyRef): string {
  return ref.family ?? ref.id;
}

/**
 * The catalog entry for a workspace's template: the pinned revision when it is
 * still listed, otherwise the newest listed revision of the same family.
 */
export function resolveTemplate<T extends FamilyRef & { revision: number }>(
  templates: readonly T[] | undefined,
  pinned: FamilyRef,
): T | undefined {
  if (!templates) return undefined;
  const exact = templates.find((tpl) => tpl.id === pinned.id);
  if (exact) return exact;
  const family = templateFamily(pinned);
  return templates
    .filter((tpl) => templateFamily(tpl) === family)
    .reduce<T | undefined>((best, tpl) => (!best || tpl.revision > best.revision ? tpl : best), undefined);
}
