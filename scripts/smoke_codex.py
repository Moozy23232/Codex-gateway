#!/usr/bin/env python3
"""Opt-in live Codex smoke check. It sends one small model request per alias."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--gateway-home', type=Path, required=True)
    parser.add_argument('--gateway-bin', default='codex-gateway')
    parser.add_argument('--codex-bin', default='codex')
    parser.add_argument('--model', action='append', required=True)
    parser.add_argument('--effort', help='Optional reasoning effort supported by these models')
    parser.add_argument('--timeout', type=float, default=180)
    parser.add_argument('--output-root', type=Path, help='Defaults to <gateway-home>/runs/_tests, outside the source checkout')
    args = parser.parse_args()
    gateway = shutil.which(args.gateway_bin)
    codex = shutil.which(args.codex_bin)
    if not gateway or not codex:
        parser.error('Both gateway and Codex executables must be installed or passed explicitly')
    args.output_root = args.output_root or args.gateway_home.resolve() / 'runs/_tests'
    args.output_root.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime('%Y%m%d-%H%M%S')
    output = Path(tempfile.mkdtemp(prefix=f'live_codex_{stamp}_', dir=args.output_root.resolve()))
    work = output / 'work'; work.mkdir()
    marker = 'CODEX_GATEWAY_SMOKE_OK'
    results = []
    for number, model in enumerate(args.model):
        case = output / f'model_{number + 1}'; case.mkdir(mode=0o700)
        command = [gateway, '--home', str(args.gateway_home.resolve()), 'run', '--codex-bin', codex,
                   '--', 'exec', '--skip-git-repo-check', '--ephemeral', '--json',
                   '--model', model, '--sandbox', 'read-only', '--output-last-message', str(case / 'answer.txt')]
        if args.effort:
            command.extend(['-c', 'model_reasoning_effort=' + json.dumps(args.effort)])
        command.append('Reply with exactly ' + marker + '. Do not use tools.')
        (case / 'command.json').write_text(json.dumps(command, indent=2) + '\n')
        started = time.monotonic()
        timed_out = False
        with (case / 'events.jsonl').open('wb') as stdout, (case / 'stderr.log').open('wb') as stderr:
            process = subprocess.Popen(command, cwd=work, stdin=subprocess.DEVNULL,
                                       stdout=stdout, stderr=stderr, start_new_session=True)
            try:
                code = process.wait(timeout=args.timeout)
            except subprocess.TimeoutExpired:
                timed_out = True
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    code = process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    code = process.wait()
        answer = (case / 'answer.txt').read_text().strip() if (case / 'answer.txt').exists() else ''
        result = {'model': model, 'exit_code': code, 'timed_out': timed_out,
                  'seconds': round(time.monotonic() - started, 3),
                  'marker_matches': answer == marker, 'passed': not timed_out and code == 0 and answer == marker}
        (case / 'exit-code.txt').write_text(str(code) + '\n')
        results.append(result)
        print(json.dumps(result), flush=True)
    report = {'kind': 'live_codex_connectivity_smoke', 'results': results,
              'passed': all(result['passed'] for result in results)}
    (output / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
    print('Report:', output / 'report.json')
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
