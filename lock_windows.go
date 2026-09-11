//go:build windows

package main

import "os"

// Windows 下暂用无锁实现（单 agent 场景足够），后续可换 LockFileEx。
func lockFile(f *os.File) error   { return nil }
func unlockFile(f *os.File) error { return nil }
