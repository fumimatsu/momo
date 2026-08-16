import pathlib
import sys
import unittest


sys.path.insert(0, str(pathlib.Path(__file__).parent))
import importlib.util


MODULE_PATH = pathlib.Path(__file__).with_name("Compare-CpuGpuArucoCapacity.py")
SPEC = importlib.util.spec_from_file_location("compare_cpu_gpu_aruco_capacity", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class CompareCpuGpuArucoCapacityTest(unittest.TestCase):
    def test_build_comparison_uses_frame_presence_and_reports_speedup(self):
        base = {
            "input": "input.mp4",
            "detectionHz": 50,
            "recognitionQuality": 0.6,
            "host": "TEST",
        }
        cpu = {
            **base,
            "decoder": "nvcodec",
            "cases": [
                {
                    "sourceCount": 4,
                    "passed": True,
                    "minimumDetectionFps": 49.8,
                    "processingMsP95": 8.0,
                    "cpuPercentP95": 40.0,
                    "workers": [{
                        "marker_ids": {"1": 300, "2": 20},
                        "marker_id_frames": {"1": 100, "2": 20},
                    }],
                }
            ],
        }
        gpu = {
            **base,
            "decoder": "nvcodec-gpu",
            "cases": [
                {
                    "sourceCount": 4,
                    "passed": True,
                    "minimumDetectionFps": 49.7,
                    "processingMsP95": 4.0,
                    "cpuPercentP95": 10.0,
                    "workers": [{
                        "marker_ids": {"1": 294, "17": 4},
                        "marker_id_frames": {"1": 98, "17": 3},
                    }],
                }
            ],
        }
        result = MODULE.build_comparison(cpu, gpu, {1, 2, 3})
        comparison = result["comparisons"][0]
        self.assertEqual(2.0, comparison["processingSpeedup"])
        self.assertEqual(75.0, comparison["cpuReductionPercent"])
        self.assertEqual({"cpu": 300, "gpu": 294}, comparison["expectedMarkerInstanceCounts"]["1"])
        self.assertEqual({"cpu": 100, "gpu": 98}, comparison["expectedMarkerFrameCounts"]["1"])
        self.assertEqual({"17": 4}, comparison["gpuDiagnosticMarkerInstanceCounts"])
        self.assertEqual({"17": 3}, comparison["gpuDiagnosticMarkerFrameCounts"])

    def test_build_comparison_accepts_batch_gpu_backend(self):
        cpu = {
            "input": "input.mp4", "detectionHz": 50, "recognitionQuality": 0.6,
            "decoder": "nvcodec", "cases": [],
        }
        gpu = {**cpu, "decoder": "nvcodec-gpu-batch"}
        result = MODULE.build_comparison(cpu, gpu, {1, 2, 3})
        self.assertEqual("nvcodec-gpu-batch", result["gpuBackend"])


if __name__ == "__main__":
    unittest.main()
