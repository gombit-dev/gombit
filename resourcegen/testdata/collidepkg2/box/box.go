// Package box is box.Box's basename twin at a different import path — see
// resourcegen/testdata/collidepkg1/box for why this pair exists.
package box

// Box is a distinct type from collidepkg1/box.Box, despite the identical
// name and basename: the two must resolve to different, uniquified aliases.
type Box string
