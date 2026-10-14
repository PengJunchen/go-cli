# Harness correction

The first report writer read the CLI status after `wait_turn` had already reaped the child and therefore wrote an incorrect `0`. The captured exception records the `waitpid` exit status as `1`; the JSON report and acceptance check are corrected transparently. Raw PTY bytes and screenshots are unchanged.
