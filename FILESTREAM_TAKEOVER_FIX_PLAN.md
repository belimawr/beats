# Filestream takeover fix plan for 8.19

## Objective

Restore the 8.19 `take_over: true` behavior for migrating registry state from
the legacy `log` input to `filestream`, and add integration coverage for the
restart-based migration scenario.

## Implementation

1. Restore the 8.19 startup hook.
   - Reintroduce `processLogInputTakeOver`.
   - Call it after `openStateStore` and before `registrar.New` in
     `filebeat/beater/filebeat.go`.
   - Adapt it to the current logger and `b.Info.Paths` APIs.
   - Reuse the existing `TakeOverLogInputStates` implementation.
   - Do not change input-reload or runner-factory behavior.

2. Add a restart-based integration test modeled on steps 1–8 of
   `TestFilebeatTakeOverFallbackWithInputReload`.
   - Create test files containing 25 lines each.
   - Start Filebeat with static `log` inputs.
   - Assert that the initial lines are emitted.
   - Stop Filebeat while preserving its data directory and registry.
   - Replace the configuration with static `filestream` inputs using
     `take_over: true`.
   - Restart Filebeat.
   - Append 25 additional lines to each file.
   - Assert that only the appended lines are emitted and counters continue
     exactly from 25.
   - Stop Filebeat. Do not test fallback or input reload.

3. Assert takeover side effects.
   - Verify that a `filestream-takeover` migration log appears.
   - Verify that a registry backup is created.
   - Verify that the original lines are not duplicated.

4. Add a Filebeat bug-fix changelog fragment.

## Verification

1. Demonstrate that the new integration test fails on the unmodified 8.19
   branch.
2. Run the takeover and beater unit tests.
3. Run only the new integration test with the `integration` build tag.
4. Run `mage check` from `filebeat/`.
