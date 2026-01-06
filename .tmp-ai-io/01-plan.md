# Integration Test Framework: Improve Log Collection on Test Failure

## Overview
Refactor the integration test framework in `libbeat/tests/integration/` to use `t.TempDir()` for temporary directories and copy the temp directory to a user-specified location when tests fail, before the test ends.

## Goals
1. Replace custom `CreateTempDir` with `t.TempDir()` (Go 1.15+ standard)
2. On test failure, copy the entire temp directory to a user-configurable location before cleanup
3. Maintain backward compatibility with existing test behavior
4. Improve debugging experience by preserving complete test artifacts

## Current State

### Temp Directory Creation
- `CreateTempDir()` (`framework.go:776-810`) uses `os.MkdirTemp()` with custom cleanup logic
- Creates temp dir in `build/integration-tests/` or `os.TempDir()`
- Cleanup function removes dir on success, preserves on failure
- On failure, logs temp dir location: `"Temporary directory saved: %s"`

### Error Reporting
- `reportErrors()` (`framework.go:1114-1140`) called from `NewBeat()` cleanup when test fails
- Reads last 1024 bytes of stderr, stdout, and log files
- Logs truncated content to test output
- Does not copy files anywhere

### Usage Pattern
- `NewBeat()` calls `CreateTempDir()` to get temp dir
- Temp dir used for Beat home, logs, config files, stdout/stderr files
- `BeatProc.TempDir()` returns the temp dir path

## Proposed Changes

### 1. Replace `CreateTempDir` with `t.TempDir()`
- Remove `CreateTempDir()` function
- Update `NewBeat()` to use `t.TempDir()` directly
- `t.TempDir()` automatically cleans up on test success
- Need to handle the `rootDir` parameter - either remove it or make it optional via env var

### 2. Add User-Configurable Failure Artifact Location
- Environment variable: `BEATS_TEST_FAILURE_ARTIFACTS_DIR` (or similar)
- If set and test fails, copy entire temp dir to specified location
- Copy should happen before `t.TempDir()` cleanup runs
- Preserve directory structure and all files

### 3. Update Cleanup Logic
- Register cleanup function that:
  - Checks if test failed (`t.Failed()`)
  - If failed and `BEATS_TEST_FAILURE_ARTIFACTS_DIR` is set:
    - Copy temp dir to destination (with test name prefix/suffix for uniqueness)
    - Log the copied location
  - Call `reportErrors()` for truncated log output (existing behavior)
- Ensure copy happens before `t.TempDir()` cleanup (order matters)

### 4. Handle Edge Cases
- Multiple tests running concurrently (unique destination names)
- Copy failures (log error, don't fail test)
- Large temp directories (consider disk space)
- Windows file locking issues (may need retry logic)

## Implementation Steps

### Step 1: Add Copy Function
- Create `copyTempDirOnFailure()` helper function
- Use `filepath.Walk()` or `io.Copy()` to recursively copy directory
- Handle errors gracefully (log, don't fail test)
- Generate unique destination name: `{testName}-{timestamp}` or similar

### Step 2: Update `NewBeat()` Function
- Replace `CreateTempDir(t, rootDir)` with `t.TempDir()`
- Remove `rootDir` parameter or make it optional/env-based
- Update cleanup registration to:
  1. Check failure + env var
  2. Copy temp dir if needed
  3. Call `reportErrors()` (existing)

### Step 3: Remove `CreateTempDir()` Function
- Delete function definition
- Check for any other callers (grep for `CreateTempDir`)

### Step 4: Update Documentation
- Update `README.md` if it mentions temp dir behavior
- Document new env var in code comments

### Step 5: Testing
- Test successful test (temp dir removed)
- Test failed test without env var (temp dir removed by `t.TempDir()`)
- Test failed test with env var (temp dir copied, then removed)
- Test concurrent tests (unique destinations)
- Test copy failure handling

## Files Affected

### Primary Changes
- `libbeat/tests/integration/framework.go`
  - Remove `CreateTempDir()` function
  - Update `NewBeat()` to use `t.TempDir()`
  - Add `copyTempDirOnFailure()` helper
  - Update cleanup logic

### Potential Changes
- `libbeat/tests/integration/README.md` - Update documentation if needed
- Any other files that call `CreateTempDir()` (need to verify)

## Testing Strategy

### Unit Tests
- Test `copyTempDirOnFailure()` function directly
- Test cleanup ordering (copy before removal)
- Test error handling (copy failures, missing permissions)

### Integration Tests
- Run existing integration tests to ensure no regressions
- Manually test with `BEATS_TEST_FAILURE_ARTIFACTS_DIR` set
- Verify artifacts are copied correctly on failure
- Verify temp dirs are cleaned up on success

### Edge Case Tests
- Concurrent test execution
- Large directories
- Insufficient disk space
- Permission issues

## Risks & Considerations

### Risks
1. **Breaking Changes**: If other code depends on `CreateTempDir()` behavior (e.g., `rootDir` parameter)
2. **Cleanup Ordering**: `t.TempDir()` cleanup runs in reverse order of registration - need to ensure copy happens first
3. **Performance**: Copying large directories on failure may slow down test cleanup
4. **Disk Space**: Multiple failed tests could fill disk if artifacts aren't cleaned manually

### Considerations
1. **Backward Compatibility**: 
   - `rootDir` parameter in `CreateTempDir()` - check if it's used elsewhere
   - If needed, support via env var: `BEATS_TEST_TEMP_ROOT_DIR`
2. **Cleanup Order**: 
   - `t.Cleanup()` functions run in LIFO order
   - Register copy cleanup BEFORE any other cleanup that might remove files
   - `t.TempDir()` registers its own cleanup, so we need to copy before that runs
3. **Naming Convention**: 
   - Destination: `{envVar}/{testName}-{timestamp}` or `{envVar}/{testName}`
   - Handle test name sanitization (remove `/` characters, etc.)
4. **Error Handling**: 
   - Copy failures should not fail the test
   - Log warnings but continue with normal cleanup
5. **Windows Compatibility**: 
   - File locking issues when copying while Beat process may still have files open
   - May need to wait/retry logic

## Open Questions

1. **Env Var Name**: What should the environment variable be named?
   - `BEATS_TEST_FAILURE_ARTIFACTS_DIR`?
   - `BEATS_TEST_ARTIFACTS_DIR`?
   - `INTEGRATION_TEST_ARTIFACTS_DIR`?

2. **Root Dir Parameter**: Should we preserve the `rootDir` functionality?
   - Option A: Remove it entirely (use `t.TempDir()` default location)
   - Option B: Support via env var `BEATS_TEST_TEMP_ROOT_DIR`
   - Option C: Keep as optional parameter, but use `t.TempDir()` if not provided

3. **Destination Naming**: How should we name copied directories?
   - `{testName}-{timestamp}` for uniqueness?
   - Just `{testName}` (overwrites on rerun)?
   - Include more context (beat name, etc.)?

4. **Copy Timing**: Should we copy immediately on failure detection, or wait until cleanup?
   - Current plan: During cleanup (when `t.Failed()` is checked)
   - Alternative: Could use `t.Cleanup()` but need to ensure it runs before `t.TempDir()` cleanup

5. **Selective Copying**: Should we copy everything or allow filtering?
   - Current plan: Copy entire directory
   - Alternative: Only copy logs, configs, and specific files

## Implementation Notes

### Cleanup Ordering Challenge
`t.TempDir()` automatically registers cleanup. We need to copy BEFORE that cleanup runs. Options:
- Register our cleanup first (LIFO means it runs last, after `t.TempDir()` cleanup)
- Use a different approach: check `t.Failed()` in a cleanup registered AFTER `t.TempDir()` cleanup
- Actually, we want to copy BEFORE removal, so we need our cleanup to run AFTER `t.TempDir()` cleanup
- But `t.TempDir()` cleanup removes the dir, so we can't copy after
- Solution: Copy in our cleanup, which should run BEFORE `t.TempDir()` cleanup
- Register our cleanup AFTER calling `t.TempDir()` so it runs first (LIFO)

### Code Structure
```go
func NewBeat(t *testing.T, beatName, binary string, args ...string) *BeatProc {
    // Use t.TempDir() instead of CreateTempDir
    tempDir := t.TempDir()
    
    // ... rest of setup ...
    
    // Register cleanup that copies on failure
    t.Cleanup(func() {
        if t.Failed() {
            copyTempDirOnFailure(t, tempDir, beatName)
        }
        reportErrors(t, tempDir, beatName)
    })
    
    return &p
}
```

### Copy Function Signature
```go
func copyTempDirOnFailure(t *testing.T, srcDir, beatName string) {
    destRoot := os.Getenv("BEATS_TEST_FAILURE_ARTIFACTS_DIR")
    if destRoot == "" {
        return
    }
    
    // Generate unique destination name
    // Copy directory recursively
    // Log result
}
```
