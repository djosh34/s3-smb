package vfs

// MaxXattrSize is the pinned JuiceFS VFS value limit (v1.4.1,
// pkg/vfs/vfs.go: xattrMaxSize). The direct metadata adapter must enforce the
// same bound; otherwise SMB could bypass the native filesystem entry point.
const MaxXattrSize = 65536
