#!/bin/sh
# Proposal-only fixture validation. The protected Makefile is patched only in
# a disposable scratch directory; this script never edits the repository copy.
set -u

SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd) || exit 1
REPO_ROOT=$(CDPATH= cd "$SCRIPT_DIR/../../.." && pwd) || exit 1
PATCH_FILE=$SCRIPT_DIR/r0-coverage-threshold-proposal.patch
TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/r0-coverage-threshold.XXXXXX") || exit 1
trap 'rm -rf "$TMP_ROOT"' 0 HUP INT TERM

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

pass() {
	echo "PASS: $*"
}

for command in awk bc cp git grep make mktemp; do
	command -v "$command" >/dev/null 2>&1 || fail "required command not found: $command"
done
REAL_BC=$(command -v bc) || fail "required command not found: bc"

(
	cd "$REPO_ROOT" || exit 1
	git apply --check "$PATCH_FILE"
) || fail "proposal patch does not apply to the checked-out baseline"
pass "git apply --check against the repository baseline"

mkdir -p "$TMP_ROOT/bin" "$TMP_ROOT/base" "$TMP_ROOT/proposed" || fail "cannot create scratch directories"
cp "$REPO_ROOT/Makefile" "$TMP_ROOT/base/Makefile" || fail "cannot copy baseline Makefile"
cp "$REPO_ROOT/Makefile" "$TMP_ROOT/proposed/Makefile" || fail "cannot copy proposed Makefile"
(
	cd "$TMP_ROOT/proposed" || exit 1
	git init -q || exit 1
	git apply "$PATCH_FILE"
) || fail "cannot apply proposal patch to the scratch Makefile"

cat >"$TMP_ROOT/bin/go" <<'EOF'
#!/bin/sh
case "$1" in
	test)
		exit 0
		;;
	tool)
		[ "${2-}" = cover ] || exit 64
		cat "$FAKE_COVER_REPORT"
		exit "${FAKE_COVER_STATUS:-0}"
		;;
esac
echo "unexpected fake go invocation: $*" >&2
exit 64
EOF
chmod +x "$TMP_ROOT/bin/go" || fail "cannot install fake go command"

cat >"$TMP_ROOT/bin/bc" <<'EOF'
#!/bin/sh
cat >"$FAKE_BC_INPUT" || exit 1
exec "$REAL_BC" <"$FAKE_BC_INPUT"
EOF
chmod +x "$TMP_ROOT/bin/bc" || fail "cannot install bc input-capture wrapper"

run_case() {
	make_dir=$1
	name=$2
	expect=$3
	threshold=$4
	cover_status=$5
	report=$6
	report_file=$TMP_ROOT/report.txt
	log_file=$TMP_ROOT/$name.log
	printf '%s\n' "$report" >"$report_file" || fail "$name: cannot write fake cover report"
	if [ -n "$threshold" ]; then
		if (
			cd "$make_dir" || exit 1
			FAKE_COVER_REPORT=$report_file FAKE_COVER_STATUS=$cover_status REAL_BC=$REAL_BC FAKE_BC_INPUT=$TMP_ROOT/$name.bc-input PATH=$TMP_ROOT/bin:$PATH \
				make -s test-cov-check COVERAGE_DIR="$TMP_ROOT/coverage/$name" COVERAGE_THRESHOLD="$threshold"
		) >"$log_file" 2>&1; then
			rc=0
		else
			rc=$?
		fi
	else
		if (
			cd "$make_dir" || exit 1
			unset COVERAGE_THRESHOLD
			FAKE_COVER_REPORT=$report_file FAKE_COVER_STATUS=$cover_status REAL_BC=$REAL_BC FAKE_BC_INPUT=$TMP_ROOT/$name.bc-input PATH=$TMP_ROOT/bin:$PATH \
				make -s test-cov-check COVERAGE_DIR="$TMP_ROOT/coverage/$name"
		) >"$log_file" 2>&1; then
			rc=0
		else
			rc=$?
		fi
	fi
	case "$expect" in
		baseline_bug)
			[ "$rc" -eq 0 ] || fail "$name: expected the baseline recipe to return success, got $rc"
			grep -E 'too many arguments|unexpected operator|integer expression expected' "$log_file" >/dev/null || fail "$name: expected the baseline shell test error"
			grep -F 'Coverage check passed:' "$log_file" >/dev/null || fail "$name: expected the baseline false pass"
			expected_bc_input=$(printf '100.0\n81.5 < 65')
			actual_bc_input=$(cat "$TMP_ROOT/$name.bc-input")
			[ "$actual_bc_input" = "$expected_bc_input" ] || fail "$name: real bc did not receive the false-match and final-total expressions"
			printf '%s\n' 'Captured real bc stdin:'
			cat "$TMP_ROOT/$name.bc-input"
			printf '%s\n' 'Captured baseline make output:'
			cat "$log_file"
			pass "$name reproduces the grep false match and false pass"
			;;
		pass)
			[ "$rc" -eq 0 ] || { cat "$log_file" >&2; fail "$name: expected success, got $rc"; }
			grep -F 'Coverage check passed:' "$log_file" >/dev/null || { cat "$log_file" >&2; fail "$name: missing pass result"; }
			if [ "$name" = anchored-total-default ]; then
				grep -F 'Coverage check passed: 81.5% >= 65%' "$log_file" >/dev/null || { cat "$log_file" >&2; fail "$name: did not select the anchored 81.5% total"; }
			fi
			pass "$name"
			;;
		fail)
			[ "$rc" -ne 0 ] || { cat "$log_file" >&2; fail "$name: expected failure"; }
			if grep -F 'Coverage check passed:' "$log_file" >/dev/null; then
				cat "$log_file" >&2
				fail "$name: emitted a pass result on failure"
			fi
			case "$name" in
				above-threshold)
					grep -F 'ERROR: Coverage 81.5% is below threshold 90%' "$log_file" >/dev/null || fail "$name: wrong threshold result"
					;;
				below-threshold)
					grep -F 'ERROR: Coverage 64.9% is below threshold 65%' "$log_file" >/dev/null || fail "$name: did not use the default threshold"
					;;
				missing-total|duplicate-total|malformed-total)
					grep -F 'go tool cover output must contain exactly one valid total coverage line' "$log_file" >/dev/null || fail "$name: did not reject the malformed total report"
					;;
				cover-command-nonzero)
					grep -F 'go tool cover failed with exit code 7' "$log_file" >/dev/null || fail "$name: did not preserve the tool failure status"
					grep -E 'Error( code)? 7([[:space:]]|$)' "$log_file" >/dev/null || fail "$name: make did not report the recipe exit status 7"
					;;
				malformed-threshold)
					grep -F 'Coverage threshold must be a number between 0 and 100' "$log_file" >/dev/null || fail "$name: did not reject the invalid threshold"
					;;
				out-of-range-total)
					grep -F 'go tool cover output must contain exactly one valid total coverage line' "$log_file" >/dev/null || fail "$name: did not reject coverage above 100 percent"
					;;
			esac
			pass "$name"
			;;
		*)
			fail "$name: unknown expectation $expect"
			;;
	esac
}

false_match_report=$(printf '%s\n' '/module/total_counter.go:17: fakeFunction 100.0%' 'total: (statements) 81.5%')
run_case "$TMP_ROOT/base" baseline-false-pass baseline_bug 65 0 "$false_match_report"
run_case "$TMP_ROOT/proposed" anchored-total-default pass '' 0 "$false_match_report"
run_case "$TMP_ROOT/proposed" above-threshold fail 90 0 'total: (statements) 81.5%'
run_case "$TMP_ROOT/proposed" below-threshold fail '' 0 'total: (statements) 64.9%'
run_case "$TMP_ROOT/proposed" missing-total fail 65 0 '/module/total_counter.go:17: fakeFunction 100.0%'
run_case "$TMP_ROOT/proposed" duplicate-total fail 65 0 "$(printf '%s\n' 'total: (statements) 81.5%' 'total: (statements) 100.0%')"
run_case "$TMP_ROOT/proposed" malformed-total fail 65 0 'total: (statements) eighty-one%'
run_case "$TMP_ROOT/proposed" out-of-range-total fail 65 0 'total: (statements) 100.1%'
run_case "$TMP_ROOT/proposed" cover-command-nonzero fail 65 7 'total: (statements) 81.5%'
run_case "$TMP_ROOT/proposed" malformed-threshold fail invalid 0 'total: (statements) 81.5%'

echo "All coverage-threshold proposal fixtures passed."
