import importlib.util
from pathlib import Path
import unittest
from unittest import mock

MODULE_PATH = Path(__file__).with_name("harness.py")
SPEC = importlib.util.spec_from_file_location("accumulo_harness", MODULE_PATH)
assert SPEC and SPEC.loader
harness = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(harness)


class HarnessTest(unittest.TestCase):
    def test_static_configuration_matches_exact_target(self):
        self.assertEqual([], harness.validate())
        self.assertEqual("4.0.0-SNAPSHOT", harness.ACCUMULO_VERSION)
        self.assertEqual(
            "1a716b2c1bb5762ead4b46d2bc4f53e13873b314",
            harness.ACCUMULO_REVISION,
        )
        self.assertEqual(
            "5d7a94492832121f507029d9d7e7627fd88e95ba",
            harness.ACCUMULO_ACCESS_REVISION,
        )

    def test_compose_command_is_stable_and_project_scoped(self):
        command = harness.compose_command("up", "--detach", "--build")
        self.assertEqual("docker", command[0])
        self.assertEqual(
            [
                "compose",
                "--project-name",
                "shoal-accumulo4-test",
                "--file",
                str(harness.COMPOSE_FILE),
                "up",
                "--detach",
                "--build",
            ],
            command[1:],
        )

    def test_java_client_command_is_noninteractive(self):
        self.assertEqual(
            ["exec", "-T", "manager", "bash", "/opt/shoal-smoke/run.sh", "smoke"],
            harness.client_command("smoke")[-6:],
        )

    @mock.patch.object(harness.subprocess, "run")
    def test_compactor_smoke_has_bounded_timeout(self, run):
        with mock.patch.dict(
            harness.os.environ, {"SHOAL_ACCUMULO_COMPACTOR_TIMEOUT": "42"}
        ):
            harness.run_compactor_smoke()
        run.assert_called_once_with(
            harness.client_command("compactor"),
            text=True,
            check=True,
            timeout=42,
        )

    def test_role_commands_are_explicit_and_noninteractive(self):
        self.assertEqual(
            [
                harness.sys.executable,
                "test/accumulo/harness.py",
                "role",
                "tserver",
            ],
            harness.role_command("tserver"),
        )
        self.assertEqual(
            harness.compose_command(
                "--profile",
                "shoal",
                "up",
                "--detach",
                "--build",
                "--no-deps",
                "shoal-tserver",
                "shoal-compactor",
            ),
            harness.compose_services("shoal-tserver", "shoal-compactor"),
        )

    @mock.patch.object(harness.shutil, "rmtree")
    @mock.patch.object(harness, "run")
    def test_down_includes_profiled_role_services(self, run, rmtree):
        harness.down()
        run.assert_called_once_with(
            harness.compose_command(
                "--profile",
                "shoal",
                "down",
                "--volumes",
                "--remove-orphans",
            ),
            check=False,
        )
        rmtree.assert_called_once_with(harness.LIVE_DIR, ignore_errors=True)

    @mock.patch.object(harness, "_run_compactor_role", side_effect=RuntimeError("failed"))
    @mock.patch.object(harness, "down")
    @mock.patch.object(harness, "run")
    @mock.patch.object(harness, "require_docker")
    def test_role_failure_logs_profiled_services(
        self, _require_docker, run, down, _role
    ):
        with self.assertRaisesRegex(RuntimeError, "failed"):
            harness.role_run("compactor")
        run.assert_called_once_with(
            harness.compose_command("--profile", "shoal", "logs", "--no-color"),
            check=False,
        )
        down.assert_called_once()

    def test_static_configuration_wires_production_role_scenarios(self):
        compose = harness.COMPOSE_FILE.read_text(encoding="utf-8")
        java = (harness.HARNESS / "smoke" / "AccumuloSmoke.java").read_text(
            encoding="utf-8"
        )
        for required in (
            "shoal-tserver:",
            "shoal-compactor:",
            "-enable-ingest",
            "-storage hdfs",
            "-group",
            "shoal_default",
            "-cancel-interval",
        ):
            self.assertIn(required, compose)
        for required in (
            "shoal-ready",
            "recovery-prepare",
            "recovery-verify",
            "setBatchSize(1)",
            "cancelCompaction",
            "promotion-equivalent=true",
        ):
            self.assertIn(required, java)

    @mock.patch.object(harness, "down")
    @mock.patch.object(harness, "wait_ready", side_effect=RuntimeError("not ready"))
    @mock.patch.object(harness, "run")
    def test_failed_start_logs_and_cleans_up(self, run, _wait_ready, down):
        with self.assertRaisesRegex(RuntimeError, "not ready"):
            harness.start_and_wait()
        self.assertEqual(2, down.call_count)
        self.assertEqual(
            harness.compose_command("logs", "--no-color"),
            run.call_args_list[-1].args[0],
        )

    @mock.patch.object(harness.shutil, "which", return_value=None)
    def test_missing_docker_is_an_explicit_skip_error(self, _which):
        with self.assertRaisesRegex(
            harness.DockerUnavailable, "Docker CLI is not installed"
        ):
            harness.require_docker()


if __name__ == "__main__":
    unittest.main()
