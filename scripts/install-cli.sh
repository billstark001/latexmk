#!/usr/bin/env bash
# Bash 3.2+ installer. The generated exports also work in zsh and POSIX sh.
set -euo pipefail

die() { printf 'latexmk installer: %s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'EOF'
Usage: bash install-cli.sh [--local FILE | --release TAG] [--rc FILE ...]
                           [--prefix DIR] [--uninstall]

Default: download the latest GitHub release and register it in the login
shell's rc file (.zshrc, .bashrc, or .profile). Use --rc for custom/login rc
files; repeat it to register multiple shells. --release accepts "latest".
--local FILE registers an existing binary; rebuilding it updates the command.
--prefix DIR defaults to ${XDG_DATA_HOME:-$HOME/.local/share}/latexmk.
--uninstall removes only the managed rc blocks, retaining installed binaries.
Re-running replaces the managed registration. Unrelated rc content is kept.
EOF
}

# Quote one literal for Bash/zsh/sh without evaluating any part of the path.
quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

absolute() {
  case "$1" in /*) printf '%s\n' "$1" ;; *) printf '%s/%s\n' "$PWD" "$1" ;; esac
}

# Follow dotfile symlinks without replacing the symlink itself.
resolve_rc() {
  local name count target
  name=$(absolute "$1")
  count=0
  while [ -L "$name" ]; do
    count=$((count + 1))
    [ "$count" -le 40 ] || die "rc symlink loop: $1"
    target=$(readlink "$name")
    case "$target" in /*) name=$target ;; *) name="$(dirname "$name")/$target" ;; esac
  done
  mkdir -p "$(dirname "$name")"
  printf '%s/%s\n' "$(cd "$(dirname "$name")" && pwd -P)" "$(basename "$name")"
}

# Strip all complete managed blocks. Refuse malformed markers rather than
# guessing where the user's own shell configuration resumes.
strip_block() {
  awk '
    $0 == "# >>> latexmk CLI >>>" { if (inside) exit 2; inside=1; next }
    $0 == "# <<< latexmk CLI <<<" { if (!inside) exit 2; inside=0; next }
    !inside { print }
    END { if (inside) exit 2 }
  ' "$1"
}

download() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --connect-timeout 15 --max-time 180 --retry 3 "$@"
}

main() {
  local prefix release source uninstall arg rc shell_name work bin staged expected actual asset os arch base
  local rc_file temp original target release_requested
  local -a rc_files resolved_files
  prefix="${XDG_DATA_HOME:-$HOME/.local/share}/latexmk"
  release=latest source= uninstall=false release_requested=false
  rc_files=() resolved_files=()
  while [ "$#" -gt 0 ]; do
    arg=$1; shift
    case "$arg" in
      --help|-h) usage; return ;;
      --uninstall) uninstall=true ;;
      --local|--release|--rc|--prefix)
        [ "$#" -gt 0 ] && [ -n "$1" ] || die "$arg requires a value"
        case "$arg" in
          --local) source=$1 ;;
          --release) release=$1; release_requested=true ;;
          --rc) rc_files+=("$1") ;;
          --prefix) prefix=$1 ;;
        esac
        shift ;;
      *) die "unknown option: $arg" ;;
    esac
  done
  [ -z "$source" ] || [ "$release_requested" = false ] || die '--local and --release cannot be combined'
  case "$prefix" in *:*) die 'prefix must not contain the PATH separator (:)' ;; esac
  if [ "${#rc_files[@]}" -eq 0 ]; then
    shell_name=$(basename "${SHELL:-sh}")
    case "$shell_name" in
      zsh) rc_files+=("${ZDOTDIR:-$HOME}/.zshrc") ;;
      bash) rc_files+=("$HOME/.bashrc") ;;
      sh|dash|ksh) rc_files+=("$HOME/.profile") ;;
      *) die 'unsupported login shell; specify --rc for a Bash/zsh/sh startup file' ;;
    esac
  fi
  # Newlines cannot be represented in our line-delimited rc ownership markers.
  for arg in "$prefix" "$source" "${rc_files[@]}"; do
    case "$arg" in *$'\n'*|*$'\r'*) die 'paths must not contain newlines' ;; esac
  done
  work=$(mktemp -d "${TMPDIR:-/tmp}/latexmk-install.XXXXXXXX")
  # Expand the quoted temporary path now, so the trap also works after main returns.
  trap "rm -rf -- $(quote "$work")" EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # Validate every rc before changing binaries or registrations.
  for rc in "${rc_files[@]}"; do
    if [ "$uninstall" = true ] && [ ! -e "$rc" ] && [ ! -L "$rc" ]; then continue; fi
    rc_file=$(resolve_rc "$rc")
    [ ! -e "$rc_file" ] || [ -f "$rc_file" ] || die "rc is not a regular file: $rc_file"
    if [ -f "$rc_file" ]; then
      strip_block "$rc_file" > "$work/check" || die "malformed managed block in $rc_file"
    fi
    resolved_files+=("$rc_file")
  done

  if [ "$uninstall" = false ]; then
    prefix=$(absolute "$prefix")
    bin="$prefix/bin"
    mkdir -p "$bin"
    bin=$(cd "$bin" && pwd -P)
    staged=$(mktemp "$bin/.latexmk-install.XXXXXXXX")
    trap "rm -rf -- $(quote "$work"); rm -f -- $(quote "$staged")" EXIT
    if [ -n "$source" ]; then
      source=$(absolute "$source")
      [ -f "$source" ] && [ -x "$source" ] || die "local CLI is not executable: $source"
      source="$(cd "$(dirname "$source")" && pwd -P)/$(basename "$source")"
      [ "$source" != "$bin/latexmk" ] || die 'local source must differ from the managed command'
      # Resolve existing symlinks to prevent a link back to the managed command.
      target=$source
      while [ -L "$target" ]; do
        rc=$(readlink "$target")
        case "$rc" in /*) target=$rc ;; *) target="$(dirname "$target")/$rc" ;; esac
        target="$(cd "$(dirname "$target")" && pwd -P)/$(basename "$target")"
        [ "$target" != "$bin/latexmk" ] || die 'local source points to the managed command'
      done
      rm -f "$staged"
      ln -s "$source" "$staged"
    else
      command -v curl >/dev/null || die 'curl is required for release installation'
      os=$(uname -s); arch=$(uname -m)
      case "$os" in Darwin) os=darwin ;; Linux) os=linux ;; *) die "unsupported OS: $os" ;; esac
      case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) die "unsupported architecture: $arch" ;; esac
      base=https://github.com/billstark001/latexmk
      if [ "$release" = latest ]; then
        target=$(download --output /dev/null --write-out '%{url_effective}' "$base/releases/latest")
        case "$target" in "$base/releases/tag/"*) release=${target##*/} ;; *) die 'could not resolve latest release' ;; esac
      fi
      case "$release" in ''|*[!a-zA-Z0-9._-]*) die 'invalid release tag' ;; esac
      asset="latexmk_${os}_${arch}"
      download --output "$work/SHA256SUMS" "$base/releases/download/$release/SHA256SUMS"
      download --output "$staged" "$base/releases/download/$release/$asset"
      expected=$(awk -v name="$asset" '$2 == name { count++; hash=$1 } END { if (count != 1) exit 1; print hash }' "$work/SHA256SUMS") \
        || die "checksum not found for $asset"
      [ "${#expected}" -eq 64 ] || die 'invalid SHA-256 checksum'
      case "$expected" in *[!a-fA-F0-9]*) die 'invalid SHA-256 checksum' ;; esac
      if command -v sha256sum >/dev/null; then
        actual=$(sha256sum "$staged" | awk '{print $1}')
      elif command -v shasum >/dev/null; then
        actual=$(shasum -a 256 "$staged" | awk '{print $1}')
      else
        die 'sha256sum or shasum is required'
      fi
      [ "$actual" = "$expected" ] || die 'download checksum mismatch; existing CLI was not replaced'
      chmod 755 "$staged"
    fi
    [ ! -d "$bin/latexmk" ] || die "managed command is a directory: $bin/latexmk"
    mv -f "$staged" "$bin/latexmk"
    {
      printf '%s\n' '# >>> latexmk CLI >>>'
      printf 'export LATEXMK_CLI=%s\n' "$(quote "$bin/latexmk")"
      # Re-sourcing the rc must not repeatedly prepend the same PATH entry.
      printf 'case ":$PATH:" in\n  *:%s:*) ;;\n  *) export PATH=%s:"$PATH" ;;\nesac\n' "$(quote "$bin")" "$(quote "$bin")"
      printf '%s\n' '# <<< latexmk CLI <<<'
    } > "$work/block"
  fi

  for rc_file in ${resolved_files[@]+"${resolved_files[@]}"}; do
    [ "$uninstall" = false ] || [ -f "$rc_file" ] || continue
    original="$work/original"
    if [ -f "$rc_file" ]; then cp -p "$rc_file" "$original"; else : > "$original"; fi
    if [ "$uninstall" = true ] && ! grep -qxF '# >>> latexmk CLI >>>' "$original"; then continue; fi
    strip_block "$original" > "$work/updated" || die "malformed managed block in $rc_file"
    if [ "$uninstall" = false ]; then cat "$work/block" >> "$work/updated"; fi
    if cmp -s "$original" "$work/updated"; then continue; fi
    temp=$(mktemp "$(dirname "$rc_file")/.latexmk-rc.XXXXXXXX")
    cp -p "$original" "$temp"
    cat "$work/updated" > "$temp"
    if [ -f "$rc_file" ] && ! cmp -s "$original" "$rc_file"; then
      rm -f "$temp"
      die "rc changed during installation; rerun: $rc_file"
    fi
    mv -f "$temp" "$rc_file"
    printf 'Updated %s\n' "$rc_file"
  done
  if [ "$uninstall" = true ]; then
    printf 'Registration removed. Open a new shell to discard its old environment. Installed binaries were retained.\n'
  else
    printf 'Registered %s. Open a new shell, or source your rc file, then run: latexmk version\n' "$bin/latexmk"
  fi
}

main "$@"
