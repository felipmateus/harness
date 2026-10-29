//go:build e2e && harness_testhooks

package e2e

// buildTags are the tags the harness binary of this run is built with. With
// `-tags "e2e harness_testhooks"` the whole suite runs against the hook-enabled binary, which
// must behave exactly like the plain one when HARNESS_TESTHOOKS is unset.
const buildTags = "harness_testhooks"
