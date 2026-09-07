#!/bin/sh
# The Forge git guard. Installed on a worker agent's PATH ahead of the real git
# by internal/gitguard; every git command an agent runs arrives here first.
#
# It denies exactly one thing: rewriting the remote named `origin` in a config
# file the current checkout does not own — the anvil's, reached from a worker
# either implicitly (every worker is a linked worktree, and git writes remotes
# to the shared config behind it) or by naming it. Everything else — every other
# remote name, every read, and every other git command there is — is handed to
# the real git unchanged, and a probe is only run for the handful of argument
# shapes that could be the denied one.
#
# The copy under ~/.forge is generated. Edit internal/gitguard/shim.sh.

REAL_GIT='@@FORGE_REAL_GIT@@'

# writes_shared_config <n-globals> <argv...> — true when the config file this
# command would write is one the current checkout does not own: the anvil named
# by FORGE_GUARDED_GIT_DIR, or the shared repository behind any linked worktree.
#
# Two rules rather than one because they close different doors. The second is
# the accident — a command typed with no path in it at all, run from inside a
# worker, whose write silently lands one directory up. The first is the same
# write aimed at the anvil by name (`git -C <anvil> remote set-url origin …`),
# which the worktree test cannot see: the anvil is a main checkout, so it owns
# its config, and only knowing WHICH repository is the anvil distinguishes it
# from the scratch clone an agent is welcome to repoint. The operator's own
# repair command is the same command — and never reaches here, because this
# guard is on the agent's PATH and nothing else's.
#
# The global options are replayed to the probe because `-C dir` and `--git-dir`
# decide which repository the command lands on, and a probe that dropped them
# would answer about the process's working directory instead.
#
# Any failure — git too old for --path-format, not a repository at all — is a
# false. The guard stands against an accident, and one that cannot tell what it
# is looking at must not stand between an agent and its work.
writes_shared_config() {
	g=$1
	shift
	total=$#
	i=0
	# Rotate the globals to the front of "$@" and drop everything after them:
	# shift each argument off, re-append only the ones being kept.
	while [ "$i" -lt "$total" ]; do
		arg=$1
		shift
		if [ "$i" -lt "$g" ]; then
			set -- "$@" "$arg"
		fi
		i=$((i + 1))
	done

	dirs=$("$REAL_GIT" "$@" rev-parse --path-format=absolute --git-dir --git-common-dir 2>/dev/null) || return 1
	# Split in the shell rather than through sed or head: this runs in front of
	# every git command a worker makes, and the containers it runs in are not
	# guaranteed to carry either.
	gitdir=
	common=
	{
		read -r gitdir
		read -r common
	} <<EOF
$dirs
EOF
	[ -n "$gitdir" ] && [ -n "$common" ] || return 1

	if [ -n "${FORGE_GUARDED_GIT_DIR:-}" ] && [ "$common" = "$FORGE_GUARDED_GIT_DIR" ]; then
		return 0
	fi
	[ "$gitdir" != "$common" ]
}

# writes_origin <n-globals> <argv...> — true when the command writes
# `remote.origin.url` or `remote.origin.pushurl` into the repository's own
# config. Those two and no others: `remote.origin.fetch` is the refspec the
# deployment's bootstrap widens, and nothing under `origin` but the two URLs
# decides where a fetch goes.
#
# Only `origin` is guarded. It is the remote every Forge fetch, PR reconcile and
# dependency scan resolves, so it is the one whose loss blinds the daemon. A
# worker adding a fork remote under some other name — which is what `gh pr
# checkout` of a fork does — cannot do that, and is left alone.
writes_origin() {
	g=$1
	shift
	i=0
	while [ "$i" -lt "$g" ]; do
		shift
		i=$((i + 1))
	done
	sub=$1
	shift

	case "$sub" in
	remote)
		# The verb is the first non-option token. `git remote -v` has none of
		# the verbs below and falls through as the read it is.
		verb=
		for arg in "$@"; do
			case "$arg" in
			-*) ;;
			*)
				verb=$arg
				break
				;;
			esac
		done
		# These write remote.* into the config file (`rm` is `remove`'s alias).
		# `prune`, `set-head`, `update`, `show`, `get-url` and `-v` move refs or
		# nothing at all.
		case "$verb" in
		add | remove | rm | rename | set-url | set-branches) ;;
		*) return 1 ;;
		esac
		for arg in "$@"; do
			if [ "$arg" = origin ]; then
				return 0
			fi
		done
		return 1
		;;
	config)
		# A read, or a write aimed at a config file that is not this
		# repository's, is none of the guard's business.
		for arg in "$@"; do
			case "$arg" in
			--get | --get-all | --get-regexp | --get-urlmatch | --list | -l | \
				--file | -f | --blob | --global | --system | --worktree | --edit | -e)
				return 1
				;;
			esac
		done
		# The first two positionals. git 2.46 added `config set <key> <value>`
		# beside the historical `config <key> <value>`; both reach the same file,
		# so both are read here.
		first=
		second=
		for arg in "$@"; do
			case "$arg" in
			-*) continue ;;
			esac
			if [ -z "$first" ]; then
				first=$arg
			elif [ -z "$second" ]; then
				second=$arg
			fi
		done
		written=0
		case "$first" in
		set | unset)
			key=$second
			written=1
			;;
		get | list | edit | rename-section | remove-section)
			return 1
			;;
		*)
			key=$first
			# `git config remote.origin.url` on its own prints the value.
			if [ -n "$second" ]; then
				written=1
			else
				for arg in "$@"; do
					case "$arg" in
					--unset | --unset-all | --replace-all | --add) written=1 ;;
					esac
				done
			fi
			;;
		esac
		[ "$written" -eq 1 ] || return 1

		# Split `<section>.<subsection>.<variable>`, where the subsection may
		# itself contain dots (a remote may be named `a.b`), so the section is
		# up to the FIRST dot and the variable after the LAST.
		section=${key%%.*}
		rest=${key#*.}
		variable=${rest##*.}
		subsection=${rest%.*}

		# Git folds the section and the variable to lower case and leaves the
		# subsection alone, so `Remote.origin.URL` and `remote.origin.url` are
		# one key and `remote.ORIGIN.url` is a different remote (verified
		# against git 2.43: writing the first changed the second's value, the
		# third added a `[remote "ORIGIN"]` section of its own). Compared
		# literally, `git config remote.origin.URL <path>` walked straight
		# through this guard. The bracket spellings are how a POSIX `case`
		# folds case without `tr`, which the guard must not depend on: it runs
		# in front of every git command a worker makes, in containers that do
		# not all carry one.
		case "$section" in
		[Rr][Ee][Mm][Oo][Tt][Ee]) ;;
		*) return 1 ;;
		esac
		[ "$subsection" = origin ] || return 1
		case "$variable" in
		[Uu][Rr][Ll] | [Pp][Uu][Ss][Hh][Uu][Rr][Ll]) return 0 ;;
		*) return 1 ;;
		esac
		;;
	esac
	return 1
}

# Find the subcommand, counting the global options in front of it. Anything that
# is not a `remote` or `config` invocation leaves here without a probe having
# been run, which is every git command on a worker's hot path.
sub=
n=0
skip=0
for arg in "$@"; do
	n=$((n + 1))
	if [ "$skip" -eq 1 ]; then
		skip=0
		continue
	fi
	case "$arg" in
	-C | -c | --git-dir | --work-tree | --namespace | --config-env | --super-prefix)
		skip=1
		;;
	-*) ;;
	*)
		sub=$arg
		break
		;;
	esac
done

case "$sub" in
remote | config)
	globals=$((n - 1))
	if writes_origin "$globals" "$@" && writes_shared_config "$globals" "$@"; then
		cat >&2 <<'MSG'
forge: refusing to rewrite `origin` in a repository this checkout does not own.

git keeps remotes in the SHARED config of the repository a worktree belongs to,
not in the worktree, so this write does not land where it was typed — it
repoints the anvil's origin for the daemon and for every other worker, and it
stays repointed after this worktree is deleted. An anvil went a day unfetched,
unscanned and with no PR reconciled that way.

Reads are unaffected: `git remote -v`, `git remote get-url origin`,
`git config --get remote.origin.url`. So is every remote that is not `origin`.
If you need an upstream of your own, take a clone that owns its config. Drop
the variables Forge exports into this worker first, or git answers from the
worktree wherever you cd to and the clone is never reached:

  unset GIT_DIR GIT_WORK_TREE
  git clone . /tmp/scratch && cd /tmp/scratch
MSG
		exit 1
	fi
	;;
esac

exec "$REAL_GIT" "$@"
