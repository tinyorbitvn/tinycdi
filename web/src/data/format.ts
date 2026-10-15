/**
 * Shortened retained-data id for headings and narrow places, e.g.
 * `rd_01J4Z9W2PFK8G4TQ3M7H1R5N0A` → `rd_01J4…0A`. The full id stays
 * available in the detail list (with a copy button).
 */
export function shortId(id: string): string {
  return id.length <= 10 ? id : `${id.slice(0, 7)}…${id.slice(-2)}`;
}
