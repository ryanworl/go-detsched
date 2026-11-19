// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || (js && wasm) || plan9 || wasip1

// Unix environment variables.

package syscall

import (
	"internal/goexperiment"
	"runtime"
	"sync"
)

var (
	// envLock guards env and envs.
	envLock sync.RWMutex

	// env maps from an environment variable to its first occurrence in envs.
	env map[string]int

	// envs is provided by the runtime. elements are expected to
	// be of the form "key=value". An empty string means deleted
	// (or a duplicate to be ignored).
	envs []string = runtime_envs()
)

func runtime_envs() []string // in package runtime

func runtimeEnvRead(key string) // in package runtime, which records the names looked up in det mode

var copyenv = sync.OnceFunc(func() {
	hint := 0
	if goexperiment.DetSched {
		hint = cap(envs) // fixed by the runtime; see detEnvsCopy
	}
	env = make(map[string]int, hint)
	for i, s := range envs {
		for j := 0; j < len(s); j++ {
			if s[j] == '=' {
				key := s[:j]
				if _, ok := env[key]; !ok {
					env[key] = i // first mention of key
				} else {
					// Clear duplicate keys. This permits Unsetenv to
					// safely delete only the first item without
					// worrying about unshadowing a later one,
					// which might be a security problem.
					envs[i] = ""
				}
				break
			}
		}
	}
})

func Unsetenv(key string) error {
	copyenv()

	envLock.Lock()
	defer envLock.Unlock()

	if i, ok := env[key]; ok {
		envs[i] = ""
		delete(env, key)
	}
	runtimeUnsetenv(key)
	return nil
}

func Getenv(key string) (value string, found bool) {
	copyenv()
	if len(key) == 0 {
		return "", false
	}
	if goexperiment.DetSched {
		runtimeEnvRead(key)
	}

	envLock.RLock()
	defer envLock.RUnlock()

	i, ok := env[key]
	if !ok {
		return "", false
	}
	s := envs[i]
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[i+1:], true
		}
	}
	return "", false
}

func Setenv(key, value string) error {
	copyenv()
	if len(key) == 0 {
		return EINVAL
	}
	for i := 0; i < len(key); i++ {
		if key[i] == '=' || key[i] == 0 {
			return EINVAL
		}
	}
	// On Plan 9, null is used as a separator, eg in $path.
	if runtime.GOOS != "plan9" {
		for i := 0; i < len(value); i++ {
			if value[i] == 0 {
				return EINVAL
			}
		}
	}

	envLock.Lock()
	defer envLock.Unlock()

	i, ok := env[key]
	kv := key + "=" + value
	if ok {
		envs[i] = kv
	} else {
		i = len(envs)
		envs = append(envs, kv)
	}
	env[key] = i
	runtimeSetenv(key, value)
	return nil
}

func Clearenv() {
	copyenv()

	envLock.Lock()
	defer envLock.Unlock()

	runtimeClearenv(env)

	env = make(map[string]int)
	envs = []string{}
}

func Environ() []string {
	copyenv()
	envLock.RLock()
	defer envLock.RUnlock()
	if goexperiment.DetSched {
		return detEnviron()
	}
	a := make([]string, 0, len(envs))
	for _, env := range envs {
		if env != "" {
			a = append(a, env)
		}
	}
	return a
}

// detEnviron is Environ in det mode. It leaves out the variables that a
// search's runs, their replay lines, and a debugger's runs set
// differently: those that control det mode, named GODET*, and GODEBUG
// when all its settings are the runtime's det ones (see
// runtime.detGodebugEnv), and those a shell sets for each command, _ and
// OLDPWD, which a replay line's cd changes. So a program that ranges over
// its environment does the same work in each, and runs the same
// schedule. Getenv still finds them. The result has the capacity of envs,
// which the runtime fixes (see detEnvsCopy), so that its size does not
// depend on them either.
func detEnviron() []string {
	a := make([]string, 0, cap(envs))
	for _, env := range envs {
		if env != "" && !detHidden(env) {
			a = append(a, env)
		}
	}
	return a
}

// detHidden reports whether detEnviron leaves out env, a "key=value".
func detHidden(env string) bool {
	if len(env) >= 5 && env[:5] == "GODET" || len(env) >= 2 && env[:2] == "_=" || len(env) >= 7 && env[:7] == "OLDPWD=" {
		return true
	}
	if len(env) < 8 || env[:8] != "GODEBUG=" {
		return false
	}
	// Each setting, between commas, must start with "det" or be empty.
	for s := env[8:]; s != ""; {
		i := 0
		for i < len(s) && s[i] != ',' {
			i++
		}
		if f := s[:i]; f != "" && (len(f) < 3 || f[:3] != "det") {
			return false
		}
		s = s[min(i+1, len(s)):]
	}
	return true
}
