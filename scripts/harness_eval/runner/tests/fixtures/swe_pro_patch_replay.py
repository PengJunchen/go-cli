"""PatchReplayAgent: re-grade a previous job's captured ``agent/model.patch`` in a FRESH sandbox.

Closes verifier tampering: the agent's sandbox (where it may have shimmed python3/pytest/git, edited
node_modules, planted hooks, ...) is discarded; only its git diff is applied to a pristine image and
the standard verifier runs there. Usage::

    PYTHONPATH=v2/tooling harbor run -p v2/tasks -e modal \
        -a patch_replay:PatchReplayAgent --model replay \
        --ak source_job=jobs/<agent-job> --job-name <agent-job>-regrade

Trials whose source patch is missing/empty apply nothing (reward should be 0).
"""
from __future__ import annotations

import glob
import json
import shlex
from pathlib import Path

from harbor.agents.base import BaseAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext


class PatchReplayAgent(BaseAgent):
    def __init__(self, *args, source_job: str, patch_name: str = "model.patch", **kwargs):
        super().__init__(*args, **kwargs)
        self._source_job = Path(source_job)
        self._patch_name = patch_name

    @staticmethod
    def name() -> str:
        return "patch-replay-agent"

    def version(self) -> str:
        return "1.0.0"

    async def setup(self, environment: BaseEnvironment) -> None:
        pass

    def _find_patch(self, task_name: str) -> Path | None:
        for rj in glob.glob(str(self._source_job / "instance_*" / "result.json")):
            try:
                if json.load(open(rj))["task_name"].split("/")[-1] == task_name:
                    p = Path(rj).parent / "agent" / self._patch_name
                    return p if p.exists() else None
            except Exception:
                continue
        return None

    async def run(self, instruction: str, environment: BaseEnvironment, context: AgentContext) -> None:
        # task name is recoverable from the trial logs dir name prefix; harbor passes it in context when available
        try:  # trial dir holds config.json with the task path
            task_name = Path(json.load(open(self.logs_dir.parent / "config.json"))["task"]["path"]).name
        except Exception:
            task_name = self.logs_dir.parent.name
        patch = self._find_patch(task_name)
        self.logs_dir.mkdir(parents=True, exist_ok=True)
        if patch is None or patch.stat().st_size == 0:
            self.logger.warning(f"no source patch for {task_name}; applying nothing")
            (self.logs_dir / "replay.json").write_text(json.dumps({"task": task_name, "patch": None}))
            return
        await environment.upload_file(source_path=patch, target_path="/tmp/replay.patch")
        r = await environment.exec(
            command="cd /app 2>/dev/null || cd /testbed; git apply --verbose /tmp/replay.patch "
                    "|| git apply --3way /tmp/replay.patch || patch --fuzz=3 -p1 -i /tmp/replay.patch",
            timeout_sec=300)
        st = await environment.exec(
            command="cd /app 2>/dev/null || cd /testbed; git --no-pager diff --stat | tail -n 40; echo ---; git status --short | wc -l",
            timeout_sec=120)
        (self.logs_dir / "replay.json").write_text(json.dumps(
            {"task": task_name, "patch": str(patch), "apply_rc": r.return_code,
             "apply_out": ((r.stdout or "") + (r.stderr or ""))[-6000:],
             "post_apply_diffstat": (st.stdout or "")[-3000:]}, indent=1))
