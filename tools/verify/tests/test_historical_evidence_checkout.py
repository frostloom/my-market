"""Checkout rules for byte-locked offline evidence. No model or network calls."""
import importlib.util
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parent.parent  # R2.6-T2: 仓库根（顶层 eval/ 所在）；R2.7 起 ROOT=tools/verify
SUMMARIZE = ROOT / 'summarize-offline-intent-audit.py'

# Working-tree bytes compared with locked hashes by the CI verify commands.
# 路径为仓库根相对路径（R2.6-T2 后 eval/ 位于仓库顶层；R2.7 服务拆分后校验器驻
# tools/verify/、web 生成器驻 services/gateway/web/scripts/，-text 规则已同步重锚）。
LOCKED_PATHS = (
    'eval/ai-gateway/paired-pilots/2026-10-17-jev-acceptance-v1/attempt-01/run.jsonl',
    'eval/ai-gateway/paired-pilots/2026-10-17-four-task-retest-v1/attempt-01/run.jsonl',
    'eval/ai-gateway/intent-suite/manifest.json',
    'eval/ai-gateway/intent-suite/balance.json',
    'services/gateway/web/scripts/gen-intent-suite.mjs',
    'tools/verify/customer-service-contract.py',
    'tools/verify/summarize-offline-intent-audit.py',
)


def load_summarize():
    spec = importlib.util.spec_from_file_location('summarize_offline_intent_audit', SUMMARIZE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class HistoricalEvidenceCheckoutTests(unittest.TestCase):
    def test_locked_sources_are_not_text_converted(self):
        completed = subprocess.run(
            ['git', 'check-attr', 'text', '--', *LOCKED_PATHS],
            cwd=REPO, check=True, capture_output=True, text=True)
        for line in completed.stdout.splitlines():
            path, _, value = line.split(': ')
            self.assertEqual(value, 'unset', path)

    def test_windows_source_path_stays_inside_repo(self):
        tool = load_summarize()
        relative = r'eval\experiments\2026-10-08-jev-fast-path\raw\revised-full-nojev\intent.json.gz'
        path = tool.evidence_path(relative)
        self.assertEqual(path, tool.EVAL / 'experiments/2026-10-08-jev-fast-path/raw/revised-full-nojev/intent.json.gz')
        self.assertTrue(path.is_relative_to(tool.EVAL) or path.is_relative_to(tool.ROOT))


if __name__ == '__main__':
    unittest.main()
