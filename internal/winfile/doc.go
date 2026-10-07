// Package winfile holds the Windows file primitives more than one package
// needs: the POSIX-semantics rename by handle (FileRenameInfoEx), the
// classification of a volume that refuses it, and the long-path form raw
// Win32 calls need. Each caller opens its own handle and chooses its own
// fallback for a volume without POSIX semantics: storage/local falls back to
// os.Rename, internal/atomicfile refuses unless its caller allows a
// non-atomic replace. Everything else in the package is Windows-only.
package winfile
