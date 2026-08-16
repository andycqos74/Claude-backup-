package agent

import "os"

// walkFunc is called for each entry visited by walkBackup. It mirrors
// filepath.WalkDir's contract but delivers an os.FileInfo directly: return
// fs.SkipDir from a directory to skip its contents, or fs.SkipAll to stop
// the whole walk.
type walkFunc func(path string, info os.FileInfo, err error) error

// openForBackup, lstatForBackup, walkBackup and enableBackupPrivilege have
// platform-specific implementations:
//
//   - On Windows they use SeBackupPrivilege + FILE_FLAG_BACKUP_SEMANTICS so
//     the agent can read files and traverse directories whose ACLs deny the
//     service account (as dedicated backup software does).
//   - Elsewhere they are thin wrappers over the standard os / filepath.
