# Harness interruption note

This run stopped before the first PPTX turn could reach Magpie. The durable session store has only the real `plain-live-text` user/assistant turn and terminal checkpoint; the Magpie route and usage audit has exactly one completed `codex/gpt-6.1-sol` inference (response `chatcmpl-resp_0f8f760a92d312b5016acf45cba168819bb888b444304de815`, 8,980 uncached input / 13 output tokens). The workspace contains no generated PPTX or model-written script.

The harness process was sampled while blocked in `os.write` from PTY `send_line` during the 1,751-byte PPTX prompt. SIGINT and SIGTERM to the isolated CLI did not release the master write; SIGKILL was used only on that isolated child so the harness could save its partial provider/session evidence. `run-report.json` remains failed and records CLI exit `-9`; this is a harness input-backpressure interruption and provides no evidence about PPT generation or compaction.

The saved `pty-blocked-writer.sample.txt` is the diagnostic stack sample. The runner now writes prompts nonblocking in small partial chunks while pumping PTY output and has a 90-second total write deadline; a fresh run is required to evaluate that fix.
