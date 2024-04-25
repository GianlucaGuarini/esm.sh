package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/esm-dev/esm.sh/server/storage"

	"github.com/Masterminds/semver/v3"
	"github.com/ije/gox/utils"
	"github.com/ije/gox/valid"
)

// ref https://github.com/npm/validate-npm-package-name
var npmNaming = valid.Validator{valid.FromTo{'a', 'z'}, valid.FromTo{'A', 'Z'}, valid.FromTo{'0', '9'}, valid.Eq('.'), valid.Eq('-'), valid.Eq('_')}

// NpmPackageVerions defines versions of a NPM package
type NpmPackageVerions struct {
	DistTags map[string]string         `json:"dist-tags"`
	Versions map[string]NpmPackageInfo `json:"versions"`
}

// NpmPackageJSON defines the package.json of NPM
type NpmPackageJSON struct {
	Name             string                 `json:"name"`
	Version          string                 `json:"version"`
	Type             string                 `json:"type,omitempty"`
	Main             string                 `json:"main,omitempty"`
	Browser          StringOrMap            `json:"browser,omitempty"`
	Module           StringOrMap            `json:"module,omitempty"`
	ES2015           StringOrMap            `json:"es2015,omitempty"`
	JsNextMain       string                 `json:"jsnext:main,omitempty"`
	Types            string                 `json:"types,omitempty"`
	Typings          string                 `json:"typings,omitempty"`
	SideEffects      interface{}            `json:"sideEffects,omitempty"`
	Dependencies     map[string]string      `json:"dependencies,omitempty"`
	PeerDependencies map[string]string      `json:"peerDependencies,omitempty"`
	Imports          map[string]interface{} `json:"imports,omitempty"`
	TypesVersions    map[string]interface{} `json:"typesVersions,omitempty"`
	Exports          json.RawMessage        `json:"exports,omitempty"`
	Files            []string               `json:"files,omitempty"`
	Deprecated       interface{}            `json:"deprecated,omitempty"`
	Esmsh            interface{}            `json:"esm.sh,omitempty"`
}

func (a *NpmPackageJSON) ToNpmPackage() *NpmPackageInfo {
	browser := map[string]string{}
	if a.Browser.Str != "" {
		browser["."] = a.Browser.Str
	}
	if a.Browser.Map != nil {
		for k, v := range a.Browser.Map {
			s, isStr := v.(string)
			if isStr {
				browser[k] = s
			} else {
				b, ok := v.(bool)
				if ok && !b {
					browser[k] = ""
				}
			}
		}
	}
	deprecated := ""
	if a.Deprecated != nil {
		if s, ok := a.Deprecated.(string); ok {
			deprecated = s
		}
	}
	esmsh := map[string]interface{}{}
	if a.Esmsh != nil {
		if v, ok := a.Esmsh.(map[string]interface{}); ok {
			esmsh = v
		}
	}
	var sideEffects *StringSet = nil
	sideEffectsFalse := false
	if a.SideEffects != nil {
		if s, ok := a.SideEffects.(string); ok {
			sideEffectsFalse = s == "false"
		} else if b, ok := a.SideEffects.(bool); ok {
			sideEffectsFalse = !b
		} else if m, ok := a.SideEffects.([]interface{}); ok && len(m) > 0 {
			sideEffects = newStringSet()
			for _, v := range m {
				if name, ok := v.(string); ok && endsWith(name, esExts...) {
					sideEffects.Add(name)
				}
			}
		}
	}
	var exports interface{} = nil
	if rawExports := a.Exports; rawExports != nil {
		var v interface{}
		if json.Unmarshal(rawExports, &v) == nil {
			if s, ok := v.(string); ok {
				if len(s) > 0 {
					exports = s
				}
			} else if _, ok := v.(map[string]interface{}); ok {
				om := newOrderedMap()
				if om.UnmarshalJSON(rawExports) == nil {
					exports = om
				}
			}
		}
	}
	return &NpmPackageInfo{
		Name:             a.Name,
		Version:          a.Version,
		Type:             a.Type,
		Main:             a.Main,
		Module:           a.Module.MainValue(),
		ES2015:           a.ES2015.MainValue(),
		JsNextMain:       a.JsNextMain,
		Types:            a.Types,
		Typings:          a.Typings,
		Browser:          browser,
		SideEffectsFalse: sideEffectsFalse,
		SideEffects:      sideEffects,
		Dependencies:     a.Dependencies,
		PeerDependencies: a.PeerDependencies,
		Imports:          a.Imports,
		TypesVersions:    a.TypesVersions,
		Exports:          exports,
		Files:            a.Files,
		Deprecated:       deprecated,
		Esmsh:            esmsh,
	}
}

// NpmPackage defines the package.json
type NpmPackageInfo struct {
	Name             string
	PkgName          string
	Version          string
	Type             string
	Main             string
	Module           string
	ES2015           string
	JsNextMain       string
	Types            string
	Typings          string
	SideEffectsFalse bool
	SideEffects      *StringSet
	Browser          map[string]string
	Dependencies     map[string]string
	PeerDependencies map[string]string
	Imports          map[string]interface{}
	TypesVersions    map[string]interface{}
	Exports          interface{}
	Files            []string
	Deprecated       string
	Esmsh            map[string]interface{}
}

func (a *NpmPackageInfo) UnmarshalJSON(b []byte) error {
	var n NpmPackageJSON
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*a = *n.ToNpmPackage()
	return nil
}

func getPackageInfo(wd string, name string, version string) (info NpmPackageInfo, fromPackageJSON bool, err error) {
	if name == "@types/node" {
		info = NpmPackageInfo{
			Name:    "@types/node",
			Version: nodeTypesVersion,
			Types:   "index.d.ts",
		}
		return
	}

	if wd == "" && regexpFullVersion.MatchString(version) && cfg != nil {
		wd = path.Join(cfg.WorkDir, "npm", name+"@"+version)
	}
	if wd != "" {
		pkgJsonPath := path.Join(wd, "node_modules", name, "package.json")
		if existsFile(pkgJsonPath) && parseJSONFile(pkgJsonPath, &info) == nil {
			fromPackageJSON = true
			return
		}
	}

	info, err = fetchPackageInfo(name, version)
	return
}

func fetchPackageInfo(name string, version string) (info NpmPackageInfo, err error) {
	a := strings.Split(strings.Trim(name, "/"), "/")
	name = a[0]
	if strings.HasPrefix(name, "@") && len(a) > 1 {
		name = a[0] + "/" + a[1]
	}

	if strings.HasPrefix(version, "=") || strings.HasPrefix(version, "v") {
		version = version[1:]
	}
	if version == "" {
		version = "latest"
	}

	cacheKey := fmt.Sprintf("npm:%s@%s", name, version)
	lock := getFetchLock(cacheKey)

	lock.Lock()
	defer lock.Unlock()

	// check cache firstly
	if cache != nil {
		var data []byte
		data, err = cache.Get(cacheKey)
		if err == nil && json.Unmarshal(data, &info) == nil {
			return
		}
		if err != nil && err != storage.ErrNotFound && err != storage.ErrExpired {
			log.Error("cache:", err)
		}
	}

	start := time.Now()
	defer func() {
		if err == nil {
			log.Debugf("lookup package(%s@%s) in %v", name, info.Version, time.Since(start))
		}
	}()

	isJsrScope := strings.HasPrefix(name, "@jsr/")
	url := cfg.NpmRegistry + name
	if isJsrScope {
		url = "https://npm.jsr.io/" + name
	} else if cfg.NpmRegistryScope != "" {
		isInScope := strings.HasPrefix(name, cfg.NpmRegistryScope)
		if !isInScope {
			url = "https://registry.npmjs.org/" + name
		}
	}

	isFullVersion := regexpFullVersion.MatchString(version)
	isGithubRegistry := strings.Contains(url, "npm.pkg.github.com")
	if isFullVersion && !isJsrScope && !isGithubRegistry {
		url += "/" + version
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	if cfg.NpmToken != "" && !isJsrScope {
		req.Header.Set("Authorization", "Bearer "+cfg.NpmToken)
	}
	if cfg.NpmUser != "" && cfg.NpmPassword != "" && !isJsrScope {
		req.SetBasicAuth(cfg.NpmUser, cfg.NpmPassword)
	}

	c := &http.Client{
		Timeout: 15 * time.Second,
	}
	resp, err := c.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 || resp.StatusCode == 401 {
		if isFullVersion {
			err = fmt.Errorf("npm: version %s of '%s' not found", version, name)
		} else {
			err = fmt.Errorf("npm: package '%s' not found", name)
		}
		return
	}

	if resp.StatusCode != 200 {
		ret, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("npm: could not get metadata of package '%s' (%s: %s)", name, resp.Status, string(ret))
		return
	}

	if isFullVersion && !isJsrScope {
		err = json.NewDecoder(resp.Body).Decode(&info)
		if err != nil {
			return
		}
		if cache != nil {
			cache.Set(cacheKey, mustEncodeJSON(info), 7*24*time.Hour)
		}
		return
	}

	var h NpmPackageVerions
	err = json.NewDecoder(resp.Body).Decode(&h)
	if err != nil {
		return
	}

	if len(h.Versions) == 0 {
		err = fmt.Errorf("npm: missing `versions` field")
		return
	}

	distVersion, ok := h.DistTags[version]
	if ok {
		info = h.Versions[distVersion]
	} else {
		var c *semver.Constraints
		c, err = semver.NewConstraint(version)
		if err != nil && version != "latest" {
			return fetchPackageInfo(name, "latest")
		}
		vs := make([]*semver.Version, len(h.Versions))
		i := 0
		for v := range h.Versions {
			// ignore prerelease versions
			if !strings.ContainsRune(version, '-') && strings.ContainsRune(v, '-') {
				continue
			}
			var ver *semver.Version
			ver, err = semver.NewVersion(v)
			if err != nil {
				return
			}
			if c.Check(ver) {
				vs[i] = ver
				i++
			}
		}
		if i > 0 {
			vs = vs[:i]
			if i > 1 {
				sort.Sort(semver.Collection(vs))
			}
			info = h.Versions[vs[i-1].String()]
		}
	}

	if info.Version == "" {
		err = fmt.Errorf("npm: version %s of '%s' not found", version, name)
		return
	}

	// cache package info for 10 minutes
	if cache != nil {
		cache.Set(cacheKey, mustEncodeJSON(info), 10*time.Minute)
	}
	return
}

func installPackage(dir string, pkg Pkg) (err error) {
	pkgVersionName := pkg.VersionName()
	lock := getInstallLock(pkgVersionName)

	// only one install process allowed at the same time
	lock.Lock()
	defer lock.Unlock()

	// skip install if pnpm lock file exists
	if existsFile(path.Join(dir, "pnpm-lock.yaml")) && existsFile(path.Join(dir, "node_modules", pkg.Name, "package.json")) {
		return nil
	}

	// ensure package.json file to prevent read up-levels
	packageJsonFp := path.Join(dir, "package.json")
	if !existsFile(packageJsonFp) {
		ensureDir(dir)
		err = os.WriteFile(packageJsonFp, []byte("{}"), 0644)
	}
	if err != nil {
		return fmt.Errorf("ensure package.json failed: %s", pkgVersionName)
	}

	attemptMaxTimes := 3
	for i := 1; i <= attemptMaxTimes; i++ {
		if pkg.FromGithub {
			err = os.WriteFile(packageJsonFp, []byte(fmt.Sprintf(`{"dependencies":{"%s":"github:%s#%s"}}`, pkg.Name, pkg.Name, pkg.Version)), 0644)
			if err == nil {
				err = pnpmInstall(dir)
			}
			// pnpm will ignore github package which has been installed without `package.json` file
			// so we install it manually
			if err == nil {
				packageJsonFp := path.Join(dir, "node_modules", pkg.Name, "package.json")
				if !existsFile(packageJsonFp) {
					ensureDir(path.Dir(packageJsonFp))
					err = os.WriteFile(packageJsonFp, mustEncodeJSON(pkg), 0644)
				} else {
					var p NpmPackageInfo
					err = parseJSONFile(packageJsonFp, &p)
					if err == nil && len(p.Files) > 0 {
						// install github package with ignoring `files` field
						err = ghInstall(dir, pkg.Name, pkg.Version)
					}
				}
			}
		} else if regexpFullVersion.MatchString(pkg.Version) {
			err = pnpmInstall(dir, pkgVersionName, "--prefer-offline")
		} else {
			err = pnpmInstall(dir, pkgVersionName)
		}
		packageJsonFp := path.Join(dir, "node_modules", pkg.Name, "package.json")
		if err == nil && !existsFile(packageJsonFp) {
			err = fmt.Errorf("pnpm install %s: package.json not found", pkg)
		}
		if err == nil || i == attemptMaxTimes {
			break
		}
		time.Sleep(time.Duration(i) * 100 * time.Millisecond)
	}
	return
}

func pnpmInstall(dir string, packages ...string) (err error) {
	var args []string
	if len(packages) > 0 {
		args = append([]string{"add"}, packages...)
	} else {
		args = []string{"install"}
	}
	args = append(
		args,
		"--ignore-scripts",
		"--loglevel", "error",
	)
	start := time.Now()
	cmd := exec.Command("pnpm", args...)
	cmd.Dir = dir
	if cfg.NpmToken != "" {
		cmd.Env = append(os.Environ(), "ESM_NPM_TOKEN="+cfg.NpmToken)
	}
	if cfg.NpmUser != "" && cfg.NpmPassword != "" {
		data := []byte(cfg.NpmPassword)
		password := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
		base64.StdEncoding.Encode(password, data)
		cmd.Env = append(
			os.Environ(),
			"ESM_NPM_USER="+cfg.NpmUser,
			"ESM_NPM_PASSWORD="+string(password),
		)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pnpm add %s: %s", strings.Join(packages, ","), string(output))
	}
	if len(packages) > 0 {
		log.Debug("pnpm add", strings.Join(packages, ","), "in", time.Since(start))
	} else {
		log.Debug("pnpm install in", time.Since(start))
	}
	return
}

// ref https://github.com/npm/validate-npm-package-name
func validatePackageName(name string) bool {
	scope := ""
	nameWithoutScope := name
	if strings.HasPrefix(name, "@") {
		scope, nameWithoutScope = utils.SplitByFirstByte(name, '/')
		scope = scope[1:]
	}
	if (scope != "" && !npmNaming.Is(scope)) || (nameWithoutScope == "" || !npmNaming.Is(nameWithoutScope)) || len(name) > 214 {
		return false
	}
	return true
}

// added by @jimisaacs
func toTypesPackageName(pkgName string) string {
	if strings.HasPrefix(pkgName, "@") {
		pkgName = strings.Replace(pkgName[1:], "/", "__", 1)
	}
	return "@types/" + pkgName
}

func isTypesOnlyPackage(p NpmPackageInfo) bool {
	return p.Main == "" && p.Module == "" && p.Types != ""
}

func getInstallLock(key string) *sync.Mutex {
	v, _ := installLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func getFetchLock(key string) *sync.Mutex {
	v, _ := fetchLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}
