#!/usr/bin/env python3
"""Pinned bridge to SWE-bench's official v4.1.0 evaluation harness.

This process owns no benchmark semantics of its own: it validates the
installed swebench package matches the version gremlord was built against,
then delegates to the official APIs for everything that matters (dataset
loading, test spec / image derivation, and grading). gremlord's Go code only
ever sees the JSON this bridge prints on stdout.

Four operations, selected by --op:
  check         report package/python/docker/arch prerequisites, no dataset access
  resolve       load selected instances and return official task + image metadata
  ensure_image  build missing official instance images via prepare_images.main
  grade         invoke the official evaluation harness and return its reports
"""

import argparse
import contextlib
import inspect
import json
import platform
import shutil
import subprocess
import sys
from pathlib import Path


def boolean(value: str) -> bool:
    lowered = value.lower()
    if lowered in ("true", "1", "yes"):
        return True
    if lowered in ("false", "0", "no"):
        return False
    raise argparse.ArgumentTypeError(f"invalid boolean: {value}")


def normalized_arch() -> str:
    arch = platform.machine().lower()
    if arch in ("arm64", "aarch64"):
        return "arm64"
    if arch in ("x86_64", "amd64"):
        return "x86_64"
    return arch


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser()
    p.add_argument("--op", choices=("check", "resolve", "ensure_image", "grade"), required=True)
    p.add_argument("--expected-version", required=True)
    p.add_argument("--dataset-name")
    p.add_argument("--split")
    p.add_argument("--instance-ids", nargs="*")
    p.add_argument("--namespace")
    p.add_argument("--instance-image-tag", default="latest")
    p.add_argument("--env-image-tag", default="latest")
    # SWE-bench 4.1.0's prepare_images and run_evaluation APIs build
    # x86_64 TestSpecs by default. Keep resolve on that same architecture so
    # the image key Go launches exactly matches what the official builder and
    # grader produce; Docker Desktop emulates it on Apple Silicon.
    p.add_argument("--arch", default="x86_64")
    # grade-only
    p.add_argument("--predictions-path")
    p.add_argument("--max-workers", type=int)
    p.add_argument("--run-id")
    p.add_argument("--timeout", type=int)
    p.add_argument("--cache-level", choices=("none", "base", "env", "instance"))
    p.add_argument("--clean", type=boolean)
    p.add_argument("--force-rebuild", type=boolean)
    p.add_argument("--rewrite-reports", type=boolean)
    p.add_argument("--modal", type=boolean)
    p.add_argument("--report-dir")
    return p


def load_swebench(expected_version: str):
    try:
        import swebench
    except ImportError as exc:
        raise SystemExit(f"install swebench=={expected_version}: {exc}") from exc
    if swebench.__version__ != expected_version:
        raise SystemExit(
            f"unsupported swebench version {swebench.__version__}; expected {expected_version}"
        )
    return swebench


def check_run_evaluation_api(run_evaluation) -> None:
    required = {
        "dataset_name", "split", "instance_ids", "predictions_path", "max_workers",
        "force_rebuild", "cache_level", "clean", "open_file_limit", "run_id",
        "timeout", "namespace", "rewrite_reports", "modal", "instance_image_tag",
        "env_image_tag", "report_dir",
    }
    actual = set(inspect.signature(run_evaluation).parameters)
    if not required.issubset(actual):
        raise SystemExit(f"unsupported run_evaluation API; missing {sorted(required - actual)}")


def op_check(args) -> dict:
    result = {
        "swebench_version": None,
        "swebench_ok": False,
        "python_version": platform.python_version(),
        "arch": normalized_arch(),
        "docker_ok": False,
        "docker_error": "",
        "error": "",
    }
    try:
        swebench = load_swebench(args.expected_version)
        result["swebench_version"] = swebench.__version__
        from swebench.harness.utils import load_swebench_dataset  # noqa: F401
        from swebench.harness.test_spec.test_spec import make_test_spec  # noqa: F401
        from swebench.harness.run_evaluation import main as run_evaluation

        check_run_evaluation_api(run_evaluation)
        result["swebench_ok"] = True
    except SystemExit as exc:
        result["error"] = str(exc)
        return result

    docker = shutil.which("docker")
    if not docker:
        result["docker_error"] = "docker binary not found on PATH"
        return result
    try:
        subprocess.run([docker, "info"], check=True, capture_output=True, timeout=10)
        result["docker_ok"] = True
    except Exception as exc:  # noqa: BLE001 - surfaced as a diagnostic string, not raised
        result["docker_error"] = str(exc)
    return result


def op_resolve(args) -> dict:
    load_swebench(args.expected_version)
    from swebench.harness.utils import load_swebench_dataset
    from swebench.harness.test_spec.test_spec import make_test_spec

    if not args.dataset_name or not args.split:
        raise SystemExit("resolve requires --dataset-name and --split")
    instances = load_swebench_dataset(args.dataset_name, args.split, args.instance_ids or None)
    out = []
    for instance in instances:
        spec = make_test_spec(
            instance,
            namespace=args.namespace or None,
            instance_image_tag=args.instance_image_tag,
            env_image_tag=args.env_image_tag,
            arch=args.arch,
        )
        out.append({
            "instance_id": instance["instance_id"],
            "repo": instance.get("repo", ""),
            "base_commit": instance.get("base_commit", ""),
            "problem_statement": instance.get("problem_statement", ""),
            "version": str(instance.get("version", "")),
            "instance_image_key": spec.instance_image_key,
            "env_image_key": spec.env_image_key,
            "arch": spec.arch,
            "namespace": spec.namespace or "",
        })
    return {"instances": out}


def op_ensure_image(args) -> dict:
    load_swebench(args.expected_version)
    import docker
    from swebench.harness.prepare_images import main as prepare_images
    from swebench.harness.utils import load_swebench_dataset
    from swebench.harness.test_spec.test_spec import make_test_spec

    if not args.dataset_name or not args.split or not args.instance_ids:
        raise SystemExit("ensure_image requires --dataset-name, --split, and --instance-ids")
    prepare_images(
        dataset_name=args.dataset_name,
        split=args.split,
        instance_ids=args.instance_ids,
        max_workers=args.max_workers or 1,
        force_rebuild=bool(args.force_rebuild),
        open_file_limit=4096,
        namespace=args.namespace or None,
        tag=args.instance_image_tag,
        env_image_tag=args.env_image_tag,
    )
    instances = load_swebench_dataset(args.dataset_name, args.split, args.instance_ids)
    client = docker.from_env()
    images = {}
    for instance in instances:
        spec = make_test_spec(
            instance,
            namespace=args.namespace or None,
            instance_image_tag=args.instance_image_tag,
            env_image_tag=args.env_image_tag,
            arch=args.arch,
        )
        try:
            client.images.get(spec.instance_image_key)
            images[instance["instance_id"]] = spec.instance_image_key
        except docker.errors.ImageNotFound as exc:
            raise SystemExit(f"official image build did not produce {spec.instance_image_key}: {exc}") from exc
    return {"images": images}


def op_grade(args) -> dict:
    load_swebench(args.expected_version)
    from swebench.harness.constants import RUN_EVALUATION_LOG_DIR, LOG_REPORT
    from swebench.harness.run_evaluation import main as run_evaluation
    from swebench.harness.utils import get_predictions_from_file

    check_run_evaluation_api(run_evaluation)

    if not all([args.dataset_name, args.split, args.predictions_path, args.max_workers,
                args.run_id, args.timeout is not None, args.cache_level, args.report_dir]):
        raise SystemExit("grade requires dataset-name, split, predictions-path, max-workers, "
                          "run-id, timeout, cache-level, and report-dir")

    report = run_evaluation(
        dataset_name=args.dataset_name,
        split=args.split,
        instance_ids=args.instance_ids,
        predictions_path=args.predictions_path,
        max_workers=args.max_workers,
        force_rebuild=bool(args.force_rebuild),
        cache_level=args.cache_level,
        clean=bool(args.clean),
        open_file_limit=4096,
        run_id=args.run_id,
        timeout=args.timeout,
        namespace=args.namespace or None,
        rewrite_reports=bool(args.rewrite_reports),
        modal=bool(args.modal),
        instance_image_tag=args.instance_image_tag,
        env_image_tag=args.env_image_tag,
        report_dir=args.report_dir,
    )

    # run_evaluation writes one official per-instance report.json under its
    # own log directory layout; read those back verbatim rather than
    # recomputing pass/fail from test output ourselves.
    instances = {}
    predictions = get_predictions_from_file(args.predictions_path, args.dataset_name, args.split)
    for pred in predictions:
        instance_id = pred["instance_id"]
        model = pred.get("model_name_or_path", "")
        report_path = RUN_EVALUATION_LOG_DIR / args.run_id / model.replace("/", "__") / instance_id / LOG_REPORT
        if report_path.exists():
            with open(report_path) as f:
                instances[instance_id] = json.load(f).get(instance_id, {})

    return {"report_path": str(Path(report)) if report else "", "instances": instances}



def main() -> int:
    args = parser().parse_args()
    ops = {"check": op_check, "resolve": op_resolve, "ensure_image": op_ensure_image, "grade": op_grade}
    try:
        # Official harness APIs and Hugging Face helpers print progress to
        # stdout. Keep stdout as a strict one-object JSON protocol for Go and
        # route all dependency progress/log output to stderr.
        with contextlib.redirect_stdout(sys.stderr):
            result = ops[args.op](args)
    except (SystemExit, Exception) as exc:
        print(json.dumps({"error": str(exc)}))
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
