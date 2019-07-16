// Package elisp provides a backend for Emacs Lisp using Cask.
package elisp

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/replit/upm/internal/api"
	"github.com/replit/upm/internal/util"
)

// elispSearchInfoCode is an Emacs Lisp script for searching ELPA
// databases. It is called with three command-line arguments: dir,
// action, and arg. dir is the name of a temporary directory which can
// be used as package-user-dir for package.el (normally this would be
// ~/.emacs.d/elpa). action is either "search" or "info". arg in the
// case of "search" is a search query, which is split on whitespace
// and applied conjunctively to filter the results. arg in the case of
// "info" is the name of a package for which to retrieve info. The
// script writes to stdout in JSON format, either as a map or an array
// of maps (see api.PkgInfo).
//
// Tildes are replaced by backticks before this script is executed.
const elispSearchInfoCode = `
(require 'cl-lib)
(require 'json)
(require 'map)
(require 'package)
(require 'subr-x)

;; Give MELPA priority as it has more up-to-date versions.
(setq package-archives '((melpa . "https://melpa.org/packages/")
                         (gnu . "https://elpa.gnu.org/packages/")
                         (org . "https://orgmode.org/elpa/")))

(defun upm-convert-package-desc (desc)
  "Convert package descriptor DESC to alist.
The JSON representation of the alist can be unmarshaled directly
into a PkgInfo struct in Go."
  (let ((extras (package-desc-extras desc)))
    ~((name . ,(symbol-name (package-desc-name desc)))
      (description . ,(package-desc-summary desc))
      (version . ,(package-version-join (package-desc-version desc)))
      (homepageURL . ,(alist-get :url extras))
      (author . ,(when-let ((mnt (alist-get :maintainer extras)))
                   (let ((parts nil))
                     (when-let ((email (cdr mnt)))
                       (push (format "<%s>" email) parts))
                     (when-let ((name (car mnt)))
                       (push name parts))
                     (when parts
                       (string-join parts " ")))))
      (dependencies . ,(cl-remove-if
                        (lambda (dep)
                          (string= dep "emacs"))
                        (mapcar
                         (lambda (link)
                           (symbol-name (car link)))
                         (package-desc-reqs desc)))))))

(defun upm-package-info (package)
  "Given PACKAGE string, return alist of metadata for it, or nil."
  (when-let ((descs (alist-get (intern package) package-archive-contents)))
    ;; If the same package is available from multiple repositories,
    ;; prefer the one from the repository which is listed first in
    ;; ~package-archives' (which package.el puts at the *end* of the
    ;; ~package-desc' list).
    (upm-convert-package-desc
     (car (last descs)))))

(defvar upm-num-archives-fetched 0
  "Number of package.el archives which have been fetched so far.")

(defun upm-download-callback (status archive-id action arg)
  "Callback for ~url-retrieve' on a package.el archive.
ARCHIVE-ID is a symbol (e.g. ~gnu', ~melpa', ...)."
  (cl-loop for (event data) on status by #'cddr
           do (when (eq event :error)
                (signal (car data) (cdr data))))
  (let* ((archives-dir (expand-file-name "archives" package-user-dir))
         (archive-dir (expand-file-name
                       (symbol-name archive-id) archives-dir))
         (json-encoding-pretty-print t))
    (make-directory archive-dir 'parents)
    (delete-region (point-min) url-http-end-of-headers)
    (write-file (expand-file-name "archive-contents" archive-dir))
    ;; No race condition, Elisp does not have preemptive
    ;; multithreading.
    (when (>= (cl-incf upm-num-archives-fetched) (length package-archives))
      (package-read-all-archive-contents)
      (pcase action
        ("search"
         (let ((queries (mapcar
                         #'regexp-quote (split-string arg nil 'omit-nulls))))
           (thread-last package-archive-contents
             (map-keys)
             (mapcar #'symbol-name)
             (cl-remove-if-not (lambda (package)
                                 (cl-every (lambda (query)
                                             (string-match-p query package))
                                           queries)))
             (funcall (lambda (packages)
                        (cl-sort packages #'< :key #'length)))
             (mapcar #'upm-package-info)
             (json-encode)
             (princ))
           (terpri)))
        ("info"
         (princ
          (json-encode (upm-package-info arg)))
         (terpri))
        (_ (error "No such action: %S" action))))))

(cl-destructuring-bind (dir action arg) command-line-args-left
  (setq command-line-args-left nil)
  (setq package-user-dir dir)
  (dolist (link package-archives)
    (url-retrieve
     (concat (cdr link) "archive-contents")
     #'upm-download-callback
     (list (car link) action arg)
     'silent)))

;; Wait until all the code has finished running before exiting.
(while (< upm-num-archives-fetched (length package-archives))
  ;; 50ms is small enough to be imperceptible to the user.
  (accept-process-output nil 0.05))
`

// Emacs Lisp code that Cask can evaluate which prints a list of all
// the currently installed packages to stdout, in "name=version"
// format.
const elispInstallCode = `
(dolist (dir load-path)
  (when (string-match "elpa/\\(.+\\)-\\([^-]+\\)" dir)
    (princ (format "%s=%s\n"
                   (match-string 1 dir)
                   (match-string 2 dir)))))
`

// Emacs Lisp code that Cask can evaluate in order to print a list of
// all packages from the specfile (Cask) to stdout, in "name=spec"
// format.
const elispListSpecfileCode = `
(let* ((bundle (cask-cli--bundle))
       (deps (append (cask-runtime-dependencies bundle)
                     (cask-development-dependencies bundle))))
  (dolist (d deps)
    (let ((fetcher (cask-dependency-fetcher d))
          (url (cask-dependency-url d))
          (files (cask-dependency-files d))
          (ref (cask-dependency-ref d))
          (branch (cask-dependency-branch d)))
      (princ (format "%S=%s%s%s%s\n"
                     (cask-dependency-name d)
                     (if fetcher (format "%S %S" fetcher url) "")
                     (if files (format ":files %S" files) "")
                     (if ref (format ":ref %S" ref) "")
                     (if branch (format ":branch %S" branch) ""))))))
`

// elispPatterns is the FilenamePatterns value for ElispBackend.
var elispPatterns = []string{"*.el"}

// ElispBackend is the UPM language backend for Emacs Lisp using Cask.
var ElispBackend = api.LanguageBackend{
	Name:             "elisp-cask",
	Specfile:         "Cask",
	Lockfile:         "packages.txt",
	FilenamePatterns: elispPatterns,
	Quirks:           api.QuirksNotReproducible,
	Search: func(query string) []api.PkgInfo {
		tmpdir, err := ioutil.TempDir("", "elpa")
		if err != nil {
			util.Die("%s", err)
		}
		defer os.RemoveAll(tmpdir)

		code := fmt.Sprintf("(progn %s)", elispSearchInfoCode)
		code = strings.Replace(code, "~", "`", -1)
		outputB := util.GetCmdOutput([]string{
			"emacs", "-Q", "--batch", "--eval", code,
			tmpdir, "search", query,
		})
		var results []api.PkgInfo
		if err := json.Unmarshal(outputB, &results); err != nil {
			util.Die("%s", err)
		}
		return results
	},
	Info: func(name api.PkgName) api.PkgInfo {
		tmpdir, err := ioutil.TempDir("", "elpa")
		if err != nil {
			util.Die("%s", err)
		}
		defer os.RemoveAll(tmpdir)

		code := fmt.Sprintf("(progn %s)", elispSearchInfoCode)
		code = strings.Replace(code, "~", "`", -1)
		outputB := util.GetCmdOutput([]string{
			"emacs", "-Q", "--batch", "--eval", code,
			tmpdir, "info", string(name),
		})
		var info api.PkgInfo
		if err := json.Unmarshal(outputB, &info); err != nil {
			util.Die("%s", err)
		}
		return info
	},
	Add: func(pkgs map[api.PkgName]api.PkgSpec) {
		contentsB, err := ioutil.ReadFile("Cask")
		var contents string
		if os.IsNotExist(err) {
			contents = `(source melpa)
(source gnu)
(source org)
`
		} else if err != nil {
			util.Die("Cask: %s", err)
		} else {
			contents = string(contentsB)
		}

		// Ensure newline before the stuff we add, for
		// readability.
		if len(contents) > 0 && contents[len(contents)-1] != '\n' {
			contents += "\n"
		}

		for name, spec := range pkgs {
			contents += fmt.Sprintf(`(depends-on "%s"`, name)
			if spec != "" {
				contents += fmt.Sprintf(" %s", spec)
			}
			contents += fmt.Sprint(")\n")
		}

		contentsB = []byte(contents)
		util.ProgressMsg("write Cask")
		util.TryWriteAtomic("Cask", contentsB)
	},
	Remove: func(pkgs map[api.PkgName]bool) {
		contentsB, err := ioutil.ReadFile("Cask")
		if err != nil {
			util.Die("Cask: %s", err)
		}
		contents := string(contentsB)

		for name, _ := range pkgs {
			contents = regexp.MustCompile(
				fmt.Sprintf(
					`(?m)^ *\(depends-on +"%s".*\)\n?$`,
					regexp.QuoteMeta(string(name)),
				),
			).ReplaceAllLiteralString(contents, "")
		}

		contentsB = []byte(contents)
		util.ProgressMsg("write Cask")
		util.TryWriteAtomic("Cask", contentsB)
	},
	Install: func() {
		util.RunCmd([]string{"cask", "install"})
		outputB := util.GetCmdOutput(
			[]string{"cask", "eval", elispInstallCode},
		)
		util.TryWriteAtomic("packages.txt", outputB)
	},
	ListSpecfile: func() map[api.PkgName]api.PkgSpec {
		outputB := util.GetCmdOutput(
			[]string{"cask", "eval", elispListSpecfileCode},
		)
		pkgs := map[api.PkgName]api.PkgSpec{}
		for _, line := range strings.Split(string(outputB), "\n") {
			if line == "" {
				continue
			}
			fields := strings.SplitN(line, "=", 2)
			if len(fields) != 2 {
				util.Die("unexpected output: %s", line)
			}
			name := api.PkgName(fields[0])
			spec := api.PkgSpec(fields[1])
			pkgs[name] = spec
		}
		return pkgs
	},
	ListLockfile: func() map[api.PkgName]api.PkgVersion {
		contentsB, err := ioutil.ReadFile("packages.txt")
		if err != nil {
			util.Die("packages.txt: %s", err)
		}
		contents := string(contentsB)
		r := regexp.MustCompile(`(.+)=(.+)`)
		pkgs := map[api.PkgName]api.PkgVersion{}
		for _, match := range r.FindAllStringSubmatch(contents, -1) {
			name := api.PkgName(match[1])
			version := api.PkgVersion(match[2])
			pkgs[name] = version
		}
		return pkgs
	},
	GuessRegexps: util.Regexps([]string{
		`\(\s*require\s*'\s*([^)[:space:]]+)[^)]*\)`,
	}),
	Guess: func() map[api.PkgName]bool {
		r := regexp.MustCompile(
			`\(\s*require\s*'\s*([^)[:space:]]+)[^)]*\)`,
		)
		required := map[string]bool{}
		for _, match := range util.SearchRecursive(r, elispPatterns) {
			required[match[1]] = true
		}

		r = regexp.MustCompile(
			`\(\s*provide\s*'\s*([^)[:space:]]+)[^)]*\)`,
		)
		provided := map[string]bool{}
		for _, match := range util.SearchRecursive(r, elispPatterns) {
			provided[match[1]] = true
		}

		tempdir, err := ioutil.TempDir("", "epkgs")
		if err != nil {
			util.Die("%s", err)
		}
		defer os.RemoveAll(tempdir)

		url := "https://github.com/emacsmirror/epkgs/raw/master/epkg.sqlite"
		epkgs := filepath.Join(tempdir, "epkgs.sqlite")
		util.DownloadFile(epkgs, url)

		clauses := []string{}
		for feature := range required {
			if strings.ContainsAny(feature, `\'`) {
				continue
			}
			if provided[feature] {
				continue
			}
			clauses = append(clauses, fmt.Sprintf("feature = '%s'", feature))
		}
		where := strings.Join(clauses, " OR ")
		query := fmt.Sprintf("SELECT package FROM provided PR WHERE (%s) "+
			"AND NOT EXISTS (SELECT 1 FROM builtin_libraries B "+
			"WHERE PR.feature = B.feature) "+
			"AND NOT EXISTS (SELECT 1 FROM packages PK "+
			"WHERE PR.package = PK.name AND PK.class = 'builtin');",
			where,
		)
		output := string(util.GetCmdOutput([]string{"sqlite3", epkgs, query}))

		r = regexp.MustCompile(`"(.+?)"`)
		names := map[api.PkgName]bool{}
		for _, match := range r.FindAllStringSubmatch(output, -1) {
			names[api.PkgName(match[1])] = true
		}
		return names
	},
}
