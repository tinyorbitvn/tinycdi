// Fixture for the lint:strings check: contains a literal JSX text node, so
// check-jsx-literals.mjs must report it and exit 1. Lives under tests/ so the
// default web/src scan never sees it.
export function LiteralFixture() {
  return <p>Hello</p>;
}
