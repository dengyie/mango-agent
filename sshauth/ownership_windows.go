//go:build windows

package sshauth

func setFileOwner(path string, uid, gid int) error {
	return nil
}

func getTargetOwnership(authPath, targetUser string) (int, int) {
	return -1, -1
}
