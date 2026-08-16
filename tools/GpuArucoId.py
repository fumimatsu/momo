"""GPU-resident ArUco dictionary decoding primitives for the Marker Observer PoC."""

from __future__ import annotations

import os
import sys
import warnings
from dataclasses import dataclass
from pathlib import Path

import cv2
import numpy as np


PATCH_CELLS = 6
PIXELS_PER_CELL = 4
PATCH_SIZE = PATCH_CELLS * PIXELS_PER_CELL
_DLL_DIRECTORY_HANDLES: list[object] = []
_CUPY_MODULE = None


RESIZE_KERNEL = r"""
extern "C" __global__
void resize_luma(
    const unsigned char* src,
    int src_width,
    int src_height,
    int src_stride,
    unsigned char* dst,
    int dst_width,
    int dst_height)
{
    int x = blockDim.x * blockIdx.x + threadIdx.x;
    int y = blockDim.y * blockIdx.y + threadIdx.y;
    if (x >= dst_width || y >= dst_height) return;

    float source_x = ((float)x + 0.5f) * ((float)src_width / dst_width) - 0.5f;
    float source_y = ((float)y + 0.5f) * ((float)src_height / dst_height) - 0.5f;
    int x0 = max(0, min(src_width - 1, (int)floorf(source_x)));
    int y0 = max(0, min(src_height - 1, (int)floorf(source_y)));
    int x1 = min(src_width - 1, x0 + 1);
    int y1 = min(src_height - 1, y0 + 1);
    float fx = source_x - floorf(source_x);
    float fy = source_y - floorf(source_y);
    float top = src[y0 * src_stride + x0] * (1.0f - fx) + src[y0 * src_stride + x1] * fx;
    float bottom = src[y1 * src_stride + x0] * (1.0f - fx) + src[y1 * src_stride + x1] * fx;
    dst[y * dst_width + x] = (unsigned char)(top * (1.0f - fy) + bottom * fy + 0.5f);
}
"""


DECODE_KERNEL = r"""
#define PATCH_SIZE 24
#define PATCH_PIXELS 576
#define DICTIONARY_SIZE 50
#define ROTATIONS 4

extern "C" __global__
void decode_candidates(
    const unsigned char* image,
    int width,
    int height,
    int stride,
    const float* homographies,
    int candidate_count,
    const unsigned short* dictionary,
    int max_hamming,
    short* output_ids,
    unsigned char* output_hamming,
    unsigned char* output_border_errors,
    unsigned short* output_codes)
{
    int candidate = blockIdx.x;
    int tid = threadIdx.x;
    if (candidate >= candidate_count) return;

    __shared__ unsigned char patch[PATCH_PIXELS];
    __shared__ unsigned int histogram[256];
    __shared__ int threshold;
    __shared__ unsigned long long inner_sum;
    __shared__ unsigned long long inner_square_sum;
    __shared__ int uniform_bit;

    for (int index = tid; index < 256; index += blockDim.x) histogram[index] = 0;
    if (tid == 0) {
        inner_sum = 0;
        inner_square_sum = 0;
        uniform_bit = -1;
    }
    __syncthreads();

    const float* h = homographies + candidate * 9;
    for (int index = tid; index < PATCH_PIXELS; index += blockDim.x) {
        int x = index % PATCH_SIZE;
        int y = index / PATCH_SIZE;
        float denominator = h[6] * x + h[7] * y + h[8];
        float source_x = (h[0] * x + h[1] * y + h[2]) / denominator;
        float source_y = (h[3] * x + h[4] * y + h[5]) / denominator;
        int source_ix = max(0, min(width - 1, __float2int_rn(source_x)));
        int source_iy = max(0, min(height - 1, __float2int_rn(source_y)));
        unsigned char value = image[source_iy * stride + source_ix];
        patch[index] = value;
        atomicAdd(&histogram[value], 1);
        if (x >= 2 && x < PATCH_SIZE - 2 && y >= 2 && y < PATCH_SIZE - 2) {
            atomicAdd(&inner_sum, (unsigned long long)value);
            atomicAdd(&inner_square_sum, (unsigned long long)value * value);
        }
    }
    __syncthreads();

    if (tid == 0) {
        unsigned long long total_sum = 0;
        for (int value = 0; value < 256; ++value) total_sum += (unsigned long long)value * histogram[value];
        unsigned long long background_sum = 0;
        unsigned int background_weight = 0;
        double maximum_variance = -1.0;
        int best_threshold = 0;
        for (int value = 0; value < 256; ++value) {
            background_weight += histogram[value];
            if (background_weight == 0) continue;
            unsigned int foreground_weight = PATCH_PIXELS - background_weight;
            if (foreground_weight == 0) break;
            background_sum += (unsigned long long)value * histogram[value];
            double background_mean = (double)background_sum / background_weight;
            double foreground_mean = (double)(total_sum - background_sum) / foreground_weight;
            double difference = background_mean - foreground_mean;
            double variance = (double)background_weight * foreground_weight * difference * difference;
            if (variance > maximum_variance) {
                maximum_variance = variance;
                best_threshold = value;
            }
        }
        threshold = best_threshold;
        double inner_mean = (double)inner_sum / 400.0;
        double inner_variance = (double)inner_square_sum / 400.0 - inner_mean * inner_mean;
        if (inner_variance < 0.64) uniform_bit = inner_mean > 127.0 ? 1 : 0;
    }
    __syncthreads();

    if (tid == 0) {
        int border_errors = 0;
        unsigned short code = 0;
        for (int cell_y = 0; cell_y < 6; ++cell_y) {
            for (int cell_x = 0; cell_x < 6; ++cell_x) {
                int white_pixels = 0;
                for (int py = 0; py < 4; ++py) {
                    for (int px = 0; px < 4; ++px) {
                        int patch_x = cell_x * 4 + px;
                        int patch_y = cell_y * 4 + py;
                        white_pixels += patch[patch_y * PATCH_SIZE + patch_x] > threshold;
                    }
                }
                int bit = uniform_bit >= 0 ? uniform_bit : white_pixels > 8;
                bool border = cell_x == 0 || cell_x == 5 || cell_y == 0 || cell_y == 5;
                if (border) {
                    border_errors += bit;
                } else {
                    code = (unsigned short)((code << 1) | bit);
                }
            }
        }

        int best_id = -1;
        int best_hamming = 17;
        for (int marker_id = 0; marker_id < DICTIONARY_SIZE; ++marker_id) {
            for (int rotation = 0; rotation < ROTATIONS; ++rotation) {
                unsigned short difference = code ^ dictionary[marker_id * ROTATIONS + rotation];
                int hamming = __popc((unsigned int)difference);
                if (hamming < best_hamming) {
                    best_hamming = hamming;
                    best_id = marker_id;
                }
            }
        }
        if (border_errors > 5 || best_hamming > max_hamming) best_id = -1;
        output_ids[candidate] = (short)best_id;
        output_hamming[candidate] = (unsigned char)best_hamming;
        output_border_errors[candidate] = (unsigned char)border_errors;
        output_codes[candidate] = code;
    }
}
"""


@dataclass(frozen=True)
class GpuCandidateResult:
    marker_id: int
    hamming: int
    border_errors: int
    code: int


def load_cupy():
    global _CUPY_MODULE
    if _CUPY_MODULE is not None:
        return _CUPY_MODULE
    if os.name == "nt":
        package_root = Path(sys.prefix) / "Lib" / "site-packages" / "nvidia"
        runtime_root = package_root / "cuda_runtime"
        if runtime_root.is_dir() and not os.environ.get("CUDA_PATH"):
            os.environ["CUDA_PATH"] = str(runtime_root)
        for component in ("cuda_runtime", "cuda_nvrtc"):
            binary_directory = package_root / component / "bin"
            if binary_directory.is_dir():
                _DLL_DIRECTORY_HANDLES.append(os.add_dll_directory(str(binary_directory)))
                os.environ["PATH"] = str(binary_directory) + os.pathsep + os.environ.get("PATH", "")
    try:
        with warnings.catch_warnings():
            warnings.filterwarnings(
                "ignore",
                message="CUDA path could not be detected.*",
                category=UserWarning,
            )
            import cupy as cp
    except (ImportError, OSError) as exc:
        raise RuntimeError(
            "GPU ArUco ID PoC requires CuPy and NVRTC; run "
            "Initialize-ArucoCapacity.ps1 -IncludeNvCodec"
        ) from exc
    if cp.cuda.runtime.getDeviceCount() < 1:
        raise RuntimeError("GPU ArUco ID PoC requires an available CUDA device")
    _CUPY_MODULE = cp
    return _CUPY_MODULE


def encode_marker_bits(bits: np.ndarray) -> int:
    if bits.shape != (4, 4):
        raise ValueError(f"expected 4x4 marker bits, got {bits.shape}")
    code = 0
    for bit in bits.astype(np.uint8).flat:
        code = (code << 1) | int(bit)
    return code


def build_dictionary_patterns() -> np.ndarray:
    dictionary = cv2.aruco.getPredefinedDictionary(cv2.aruco.DICT_4X4_50)
    patterns = np.empty((50, 4), dtype=np.uint16)
    for marker_id in range(50):
        bits = cv2.aruco.Dictionary_getBitsFromByteList(
            dictionary.bytesList[marker_id : marker_id + 1], 4
        )
        for rotation in range(4):
            patterns[marker_id, rotation] = encode_marker_bits(np.rot90(bits, rotation))
    return patterns


def make_candidate_homographies(corners: np.ndarray) -> np.ndarray:
    corners = np.asarray(corners, dtype=np.float32).reshape((-1, 4, 2))
    destination = np.array(
        [[0, 0], [PATCH_SIZE - 1, 0], [PATCH_SIZE - 1, PATCH_SIZE - 1], [0, PATCH_SIZE - 1]],
        dtype=np.float32,
    )
    homographies = np.empty((len(corners), 3, 3), dtype=np.float32)
    for index, candidate in enumerate(corners):
        homographies[index] = cv2.getPerspectiveTransform(destination, candidate)
    return homographies


def marker_ids_to_mask(marker_ids: list[int]) -> int:
    mask = 0
    for marker_id in marker_ids:
        if marker_id < 0 or marker_id >= 50:
            raise ValueError(f"marker ID outside DICT_4X4_50: {marker_id}")
        mask |= 1 << marker_id
    return mask


class GpuArucoIdDecoder:
    def __init__(self, max_hamming: int = 0):
        if max_hamming < 0 or max_hamming > 16:
            raise ValueError("max_hamming must be in 0..16")
        self.cp = load_cupy()
        self.max_hamming = max_hamming
        self.dictionary = self.cp.asarray(build_dictionary_patterns().reshape(-1))
        self.resize_kernel = self.cp.RawKernel(RESIZE_KERNEL, "resize_luma")
        self.decode_kernel = self.cp.RawKernel(DECODE_KERNEL, "decode_candidates")

    @staticmethod
    def resized_shape(source_height: int, quality: float) -> tuple[int, int]:
        resized_height = max(2, int(source_height * quality))
        resized_width = max(2, int(resized_height * 16.0 / 9.0))
        return resized_height - resized_height % 2, resized_width - resized_width % 2

    def resize_nv12_luma(self, nv12_device, source_width: int, source_height: int, quality: float):
        if nv12_device.ndim != 2 or nv12_device.shape[0] < source_height or nv12_device.shape[1] < source_width:
            raise ValueError(f"unexpected NV12 device frame shape: {nv12_device.shape}")
        destination_height, destination_width = self.resized_shape(source_height, quality)
        destination = self.cp.empty((destination_height, destination_width), dtype=self.cp.uint8)
        block = (16, 16)
        grid = (
            (destination_width + block[0] - 1) // block[0],
            (destination_height + block[1] - 1) // block[1],
        )
        self.resize_kernel(
            grid,
            block,
            (
                nv12_device,
                np.int32(source_width),
                np.int32(source_height),
                np.int32(nv12_device.strides[0]),
                destination,
                np.int32(destination_width),
                np.int32(destination_height),
            ),
        )
        return destination

    def decode_device(self, gray_device, homographies_device):
        homographies_device = self.cp.asarray(homographies_device, dtype=self.cp.float32).reshape((-1, 3, 3))
        candidate_count = len(homographies_device)
        if candidate_count == 0:
            empty_ids = self.cp.empty(0, dtype=self.cp.int16)
            empty_bytes = self.cp.empty(0, dtype=self.cp.uint8)
            empty_codes = self.cp.empty(0, dtype=self.cp.uint16)
            return empty_ids, empty_bytes, empty_bytes.copy(), empty_codes
        output_ids = self.cp.empty(candidate_count, dtype=self.cp.int16)
        output_hamming = self.cp.empty(candidate_count, dtype=self.cp.uint8)
        output_border_errors = self.cp.empty(candidate_count, dtype=self.cp.uint8)
        output_codes = self.cp.empty(candidate_count, dtype=self.cp.uint16)
        self.decode_kernel(
            (candidate_count,),
            (256,),
            (
                gray_device,
                np.int32(gray_device.shape[1]),
                np.int32(gray_device.shape[0]),
                np.int32(gray_device.strides[0]),
                homographies_device,
                np.int32(candidate_count),
                self.dictionary,
                np.int32(self.max_hamming),
                output_ids,
                output_hamming,
                output_border_errors,
                output_codes,
            ),
        )
        return output_ids, output_hamming, output_border_errors, output_codes

    def decode(self, gray_device, homographies: np.ndarray) -> tuple[list[GpuCandidateResult], int]:
        homographies = np.asarray(homographies, dtype=np.float32).reshape((-1, 3, 3))
        output_ids, output_hamming, output_border_errors, output_codes = self.decode_device(
            gray_device, self.cp.asarray(homographies)
        )
        candidate_count = len(homographies)
        if candidate_count == 0:
            return [], 0
        ids = self.cp.asnumpy(output_ids)
        hamming = self.cp.asnumpy(output_hamming)
        border_errors = self.cp.asnumpy(output_border_errors)
        codes = self.cp.asnumpy(output_codes)
        results = [
            GpuCandidateResult(
                marker_id=int(ids[index]),
                hamming=int(hamming[index]),
                border_errors=int(border_errors[index]),
                code=int(codes[index]),
            )
            for index in range(candidate_count)
        ]
        valid_ids = sorted({result.marker_id for result in results if result.marker_id >= 0})
        return results, marker_ids_to_mask(valid_ids)
