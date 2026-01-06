# Integration Test Framework: Improve Log Collection on Test Failure

## Overview
Refactor the integration test framework in `libbeat/tests/integration/` to use `t.TempDir()` internally while preserving the `CreateTempDir()` public API. On test failure, copy the temp directory content to the `rootDir` location before cleanup.

## Goals
1. Preserve `CreateTempDir()` function as part of the public API
2. Internally use `t.TempDir()` for temporary directory creation (Go 1.15+ standard)
3. On test failure, copy the entire temp directory to `rootDir` (the function's argument)
4. Maintain backward compatibility with existing test behavior
5. Improve debugging experience by preserving complete test artifacts in a known location

## Current State

### Temp Directory Creation
- `CreateTempDir()` (`framework.go:776-810`) uses `os.MkdirTemp()` with custom cleanup logic
- Creates temp dir in `build/integration-tests/` or `os.TempDir()`
- Cleanup function removes dir on success, preserves on failure
- On failure, logs temp dir location: `"Temporary directory saved: %s"`

### Error Reporting
- ~~`reportErrors()` function removed~~ - No longer needed since we copy entire temp directory

### Usage Pattern
- `NewBeat()` calls `CreateTempDir()` to get temp dir
- Temp dir used for Beat home, logs, config files, stdout/stderr files
- `BeatProc.TempDir()` returns the temp dir path

## Proposed Changes

### 1. Refactor `CreateTempDir` to Use `t.TempDir()` Internally
- **Preserve** `CreateTempDir()` function signature (public API)
- Internally use `t.TempDir()` to create the actual temp directory
- `t.TempDir()` automatically cleans up on test success
- `rootDir` parameter is now used as the destination for copying artifacts on failure
- Maintain backward compatibility - function still returns temp dir path

### 2. Copy Temp Dir to `rootDir` on Failure
- When test fails, copy entire temp dir content to `rootDir`
- Create subdirectory in `rootDir` with test name (sanitized) and timestamp for uniqueness
- Format: `{rootDir}/{sanitizedTestName}-{timestamp}`
- Copy should happen before `t.TempDir()` cleanup runs
- Preserve directory structure and all files
- If `rootDir` is empty, find Beat root directory and use `build/integration-tests` as `rootDir`

### 3. Update Cleanup Logic in `CreateTempDir`
- Register cleanup function that:
  - Checks if test failed (`t.Failed()`)
  - If failed:
    - If `rootDir` is empty, find Beat root and set `rootDir = {beatRoot}/build/integration-tests`
    - Copy temp dir to `rootDir/{testName}-{timestamp}` (with sanitization)
    - Log the copied location
  - `t.TempDir()` cleanup will remove temp dir after our cleanup runs
- Ensure copy happens before `t.TempDir()` cleanup (order matters - register our cleanup after `t.TempDir()`)

### 4. Find Beat Root When `rootDir` is Empty
- Use `os.Getwd()` to get current working directory
- Walk up directory tree from current working directory to find Beat root
- Look for `main.go` file as marker (indicates Beat root directory)
- Once found, use `{beatRoot}/build/integration-tests` as `rootDir`
- Create `build/integration-tests` directory if it doesn't exist
- Fallback: if Beat root cannot be determined, use `os.TempDir()` (current behavior)

### 4. Handle Edge Cases
- Multiple tests running concurrently (unique destination names)
- Copy failures (log error, don't fail test)
- Large temp directories (consider disk space)
- Windows file locking issues (may need retry logic)

## Implementation Steps

### Step 1: Add Helper Functions
- Create `findBeatRoot()` helper function
  - Use `os.Getwd()` to get current working directory
  - Walk up directory tree looking for `main.go` file
  - Return Beat root path or empty string if not found
- Create `copyTempDirToRoot()` helper function
  - Use `filepath.Walk()` or `io.Copy()` to recursively copy directory
  - Handle errors gracefully (log, don't fail test)
  - Destination: `rootDir/{sanitizedTestName}-{timestamp}` (always include timestamp)
  - Format timestamp: `YYYYMMDD-HHMMSS` or Unix timestamp
  - Create destination directory if it doesn't exist

### Step 2: Refactor `CreateTempDir()` Function
- Keep function signature: `func CreateTempDir(t *testing.T, rootDir string) string`
- Internally call `t.TempDir()` to create the actual temp directory
- Register cleanup function that:
  1. Checks if test failed (`t.Failed()`)
  2. If failed:
     - If `rootDir` is empty, call `findBeatRoot()` and set `rootDir = {beatRoot}/build/integration-tests`
     - If `rootDir` is still empty after finding Beat root, fall back to `os.TempDir()`
     - Create `build/integration-tests` directory if it doesn't exist (when using Beat root)
     - Copy temp dir to `rootDir/{testName}-{timestamp}`
     - Log copied location
- Return temp dir path (from `t.TempDir()`)

### Step 3: Update `NewBeat()` Function
- Remove call to `reportErrors()` from cleanup (line 160)
- `NewBeat()` calls `CreateTempDir(t, rootDir)` - no other changes needed
- Ensure `rootDir` is passed correctly (currently `build/integration-tests/`)

### Step 4: Remove `reportErrors()` Function
- Delete `reportErrors()` function definition (`framework.go:1114-1140`)
- Delete `readLastNBytes()` helper function (`framework.go:1092-1112`) - only used by `reportErrors()`
- Update any documentation that references `reportErrors()`

### Step 5: Update Documentation
- Update `README.md` if it mentions temp dir behavior or `reportErrors()`
- Document new behavior in code comments

### Step 6: Testing
- Test successful test (temp dir removed)
- Test failed test with `rootDir` provided (temp dir copied to `rootDir/{testName}-{timestamp}`, then removed)
- Test failed test with empty `rootDir` (Beat root found, copied to `{beatRoot}/build/integration-tests/{testName}-{timestamp}`)
- Test failed test when Beat root cannot be found (fallback to `os.TempDir()`)
- Test concurrent tests (unique destinations with timestamps)
- Test copy failure handling
- Verify timestamp format and uniqueness

## Files Affected

### Primary Changes
- `libbeat/tests/integration/framework.go`
  - Refactor `CreateTempDir()` to use `t.TempDir()` internally
  - Add `findBeatRoot()` helper function to locate Beat root directory (looks for `main.go`)
  - Add `copyTempDirToRoot()` helper function
  - Update cleanup logic in `CreateTempDir()` to copy on failure with timestamp
  - Handle empty `rootDir` by finding Beat root and using `build/integration-tests`
  - Create `build/integration-tests` directory if it doesn't exist (when using Beat root)
  - Remove `reportErrors()` function (`framework.go:1114-1140`)
  - Remove `readLastNBytes()` helper function (`framework.go:1092-1112`)
  - Update `NewBeat()` to remove `reportErrors()` call from cleanup (line 160)

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
- Verify artifacts are copied correctly on failure
- Verify temp dirs are cleaned up on success
- Test with empty `rootDir` to verify Beat root detection works
- Test when `build/` directory doesn't exist (should be created)
- Verify `reportErrors()` removal doesn't break anything

### Edge Case Tests
- Concurrent test execution
- Large directories
- Insufficient disk space
- Permission issues

## Risks & Considerations

### Risks
1. **Semantic Change**: `rootDir` parameter changes meaning - from "where to create temp dir" to "where to copy artifacts on failure"
   - Mitigation: Temp dirs now created via `t.TempDir()` (system temp), `rootDir` only used for copying
2. **Cleanup Ordering**: `t.TempDir()` cleanup runs in reverse order of registration - need to ensure copy happens first
3. **Performance**: Copying large directories on failure may slow down test cleanup
4. **Disk Space**: Multiple failed tests could fill `rootDir` if artifacts aren't cleaned manually
5. **Empty `rootDir`**: If `rootDir` is empty string, find Beat root and use `build/integration-tests`
   - If Beat root cannot be found, fall back to `os.TempDir()` (current behavior)

### Considerations
1. **Backward Compatibility**: 
   - Function signature unchanged - `CreateTempDir(t, rootDir)` still works
   - Return value unchanged - still returns temp dir path
   - Behavior change: temp dir created via `t.TempDir()` instead of `os.MkdirTemp(rootDir, ...)`
   - If `rootDir` is empty, behavior similar to current (log location, don't copy)
2. **Cleanup Order**: 
   - `t.Cleanup()` functions run in LIFO order
   - `t.TempDir()` registers cleanup automatically
   - We register our cleanup AFTER calling `t.TempDir()`, so it runs FIRST (before temp dir removal)
   - Copy happens in our cleanup, then `t.TempDir()` cleanup removes the temp dir
3. **Naming Convention**: 
   - Destination: `{rootDir}/{sanitizedTestName}-{timestamp}` (always include timestamp)
   - Timestamp format: `YYYYMMDD-HHMMSS` or Unix timestamp (e.g., `1704067200`)
   - Sanitize test name: replace `/` with `-`, handle special characters
   - Example: `build/integration-tests/TestBase-20240101-120000`
4. **Error Handling**: 
   - Copy failures should not fail the test
   - Log warnings but continue with normal cleanup
   - Create destination directory if it doesn't exist
5. **Windows Compatibility**: 
   - File locking issues when copying while Beat process may still have files open
   - May need to wait/retry logic
6. **`rootDir` Usage**: 
   - Currently `NewBeat()` passes `build/integration-tests/` as `rootDir`
   - This becomes the artifact collection directory
   - Users can control this by modifying how they call `CreateTempDir()` or `NewBeat()`

## Open Questions

1. **Timestamp Format**: What format should we use for the timestamp?
   - `YYYYMMDD-HHMMSS` (e.g., `20240101-120000`) - human readable
   - Unix timestamp (e.g., `1704067200`) - compact, sortable
   - ISO 8601 format (e.g., `2024-01-01T12:00:00Z`) - standard but longer
   - **Decision needed**: Prefer human-readable or compact format?

2. **Test Name Sanitization**: How should we sanitize test names?
   - Replace `/` with `-` (current pattern in code: `strings.ReplaceAll(t.Name(), "/", "-")`)
   - Handle other special characters (e.g., `:`, `*`, `?`, `"`, `<`, `>`, `|`)?
   - Limit length to avoid filesystem issues?

3. **Beat Root Detection**: Use `main.go` file as marker
   - Walk up from current working directory (via `os.Getwd()`) looking for `main.go`
   - `main.go` indicates Beat root directory

4. **Copy Timing**: Confirmed - during cleanup when `t.Failed()` is checked
   - Our cleanup runs first (registered after `t.TempDir()`)
   - Copy happens before temp dir removal

5. **Selective Copying**: Copy entire directory or allow filtering?
   - **Decision**: Copy entire directory (simpler, preserves all context)
   - Future enhancement could add filtering if needed

## Implementation Notes

### Cleanup Ordering Solution
`t.TempDir()` automatically registers cleanup. We need to copy BEFORE that cleanup runs.

**Solution**: Register our cleanup AFTER calling `t.TempDir()`. Since `t.Cleanup()` runs in LIFO order (last registered, first executed), our cleanup will run FIRST, before `t.TempDir()` cleanup removes the directory.

### Code Structure

#### Updated `CreateTempDir()` Function
```go
func CreateTempDir(t *testing.T, rootDir string) string {
    // Use t.TempDir() to create temp directory
    tempDir := t.TempDir()
    
    // Register cleanup to copy on failure
    t.Cleanup(func() {
        if t.Failed() {
            // If rootDir is empty, find Beat root and use build/integration-tests
            if rootDir == "" {
                beatRoot := findBeatRoot()
                if beatRoot != "" {
                    rootDir = filepath.Join(beatRoot, "build", "integration-tests")
                    // Create build/integration-tests directory if it doesn't exist
                    if err := os.MkdirAll(rootDir, 0o750); err != nil {
                        t.Logf("[WARN] Could not create build/integration-tests directory '%s': %s", rootDir, err)
                        rootDir = os.TempDir() // Fallback
                    }
                } else {
                    // Fallback to os.TempDir() if Beat root cannot be found
                    rootDir = os.TempDir()
                }
            }
            
            // Copy temp dir to rootDir with timestamp
            copyTempDirToRoot(t, tempDir, rootDir)
        }
    })
    
    return tempDir
}
```

#### Find Beat Root Helper
```go
func findBeatRoot() string {
    // Get current working directory
    dir, err := os.Getwd()
    if err != nil {
        return ""
    }
    
    // Walk up the directory tree looking for main.go file
    for {
        // Check for main.go (indicates Beat root directory)
        mainGoPath := filepath.Join(dir, "main.go")
        if _, err := os.Stat(mainGoPath); err == nil {
            return dir
        }
        
        // Move up one directory
        parent := filepath.Dir(dir)
        if parent == dir {
            // Reached filesystem root, stop
            break
        }
        dir = parent
    }
    
    return ""
}
```

#### Copy Helper Function
```go
func copyTempDirToRoot(t *testing.T, srcDir, rootDir string) {
    // Sanitize test name
    testName := strings.ReplaceAll(t.Name(), "/", "-")
    
    // Generate timestamp (format: YYYYMMDD-HHMMSS)
    now := time.Now()
    timestamp := now.Format("20060102-150405")
    
    // Create destination directory name with timestamp
    destDirName := fmt.Sprintf("%s-%s", testName, timestamp)
    destDir := filepath.Join(rootDir, destDirName)
    
    // Create destination directory
    if err := os.MkdirAll(destDir, 0o750); err != nil {
        t.Logf("[WARN] Could not create artifact directory '%s': %s", destDir, err)
        return
    }
    
    // Copy directory recursively
    err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        
        relPath, err := filepath.Rel(srcDir, path)
        if err != nil {
            return err
        }
        
        destPath := filepath.Join(destDir, relPath)
        
        if info.IsDir() {
            return os.MkdirAll(destPath, info.Mode())
        }
        
        return copyFile(path, destPath, info.Mode())
    })
    
    if err != nil {
        t.Logf("[WARN] Could not copy temp directory '%s' to '%s': %s", srcDir, destDir, err)
        return
    }
    
    t.Logf("Test artifacts copied to: %s", destDir)
}
```

#### File Copy Helper
```go
func copyFile(src, dst string, mode os.FileMode) error {
    srcFile, err := os.Open(src)
    if err != nil {
        return err
    }
    defer srcFile.Close()
    
    dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
    if err != nil {
        return err
    }
    defer dstFile.Close()
    
    _, err = io.Copy(dstFile, srcFile)
    return err
}
```

### No Changes Needed to `NewBeat()`
`NewBeat()` already calls `CreateTempDir(t, rootDir)` - no changes required. The refactored `CreateTempDir()` maintains the same interface.
