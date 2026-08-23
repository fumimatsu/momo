"""GPU-only ArUco candidate extraction and ID detection for the Marker Observer PoC."""

from __future__ import annotations

from dataclasses import dataclass
import time
from typing import Iterable

import numpy as np

from GpuArucoId import GpuArucoIdDecoder, PATCH_SIZE


BOX_HORIZONTAL_KERNEL = r"""
extern "C" __global__
void box_horizontal(
    const unsigned char* image,
    int width,
    int height,
    int stride,
    int radius,
    unsigned int* horizontal)
{
    int x = blockDim.x * blockIdx.x + threadIdx.x;
    int y = blockDim.y * blockIdx.y + threadIdx.y;
    if (x >= width || y >= height) return;
    int left = max(0, x - radius);
    int right = min(width - 1, x + radius);
    unsigned int sum = 0;
    for (int source_x = left; source_x <= right; ++source_x) {
        sum += image[y * stride + source_x];
    }
    horizontal[y * width + x] = sum;
}
"""


THRESHOLD_INIT_KERNEL = r"""
extern "C" __global__
void threshold_and_init_labels(
    const unsigned char* image,
    int width,
    int height,
    int stride,
    const unsigned int* horizontal,
    int radius,
    int constant,
    unsigned int* labels)
{
    int x = blockDim.x * blockIdx.x + threadIdx.x;
    int y = blockDim.y * blockIdx.y + threadIdx.y;
    if (x >= width || y >= height) return;
    int top = max(0, y - radius);
    int bottom = min(height - 1, y + radius);
    unsigned int sum = 0;
    for (int source_y = top; source_y <= bottom; ++source_y) {
        sum += horizontal[source_y * width + x];
    }
    int horizontal_count = min(width - 1, x + radius) - max(0, x - radius) + 1;
    int vertical_count = bottom - top + 1;
    int count = horizontal_count * vertical_count;
    int index = y * width + x;
    labels[index] = ((int)image[y * stride + x] + constant) * count < (int)sum
        ? (unsigned int)index + 1
        : 0;
}
"""


UNION_KERNEL = r"""
__device__ __forceinline__ unsigned int find_root(unsigned int* labels, unsigned int label) {
    while (label != 0) {
        unsigned int parent = labels[label - 1];
        if (parent == label) break;
        label = parent;
    }
    return label;
}

extern "C" __global__
void union_labels(unsigned int* labels, int width, int height)
{
    int x = blockDim.x * blockIdx.x + threadIdx.x;
    int y = blockDim.y * blockIdx.y + threadIdx.y;
    if (x >= width || y >= height) return;
    int index = y * width + x;
    unsigned int own = labels[index];
    if (own == 0) return;

    const int offsets_x[4] = {-1, -1, 0, 1};
    const int offsets_y[4] = {0, -1, -1, -1};
    for (int neighbor_index = 0; neighbor_index < 4; ++neighbor_index) {
        int nx = x + offsets_x[neighbor_index];
        int ny = y + offsets_y[neighbor_index];
        if (nx < 0 || nx >= width || ny < 0) continue;
        unsigned int neighbor = labels[ny * width + nx];
        if (neighbor == 0) continue;
        unsigned int own_root = find_root(labels, own);
        unsigned int neighbor_root = find_root(labels, neighbor);
        while (own_root != neighbor_root) {
            unsigned int high = max(own_root, neighbor_root);
            unsigned int low = min(own_root, neighbor_root);
            unsigned int previous = atomicMin(&labels[high - 1], low);
            if (previous == high || previous == low) break;
            own_root = find_root(labels, low);
            neighbor_root = find_root(labels, previous);
        }
    }
}
"""


COMPRESS_KERNEL = r"""
extern "C" __global__
void compress_labels(unsigned int* labels, int count)
{
    int index = blockDim.x * blockIdx.x + threadIdx.x;
    if (index >= count || labels[index] == 0) return;
    unsigned int root = labels[index];
    while (labels[root - 1] != root) root = labels[root - 1];
    labels[index] = root;
}
"""


COMPONENT_STATS_KERNEL = r"""
extern "C" __global__
void component_stats(
    const unsigned int* labels,
    int width,
    int height,
    unsigned int* counts,
    unsigned long long* min_sum,
    unsigned long long* max_sum,
    unsigned long long* min_difference,
    unsigned long long* max_difference,
    unsigned long long* min_x,
    unsigned long long* max_x,
    unsigned long long* min_y,
    unsigned long long* max_y)
{
    int index = blockDim.x * blockIdx.x + threadIdx.x;
    int pixel_count = width * height;
    if (index >= pixel_count) return;
    unsigned int root = labels[index];
    if (root == 0) return;
    int x = index % width;
    int y = index / width;
    unsigned int sum_score = (unsigned int)(x + y);
    unsigned int difference_score = (unsigned int)(x - y + height);
    unsigned long long sum_key = ((unsigned long long)sum_score << 32) | (unsigned int)index;
    unsigned long long difference_key =
        ((unsigned long long)difference_score << 32) | (unsigned int)index;
    unsigned long long x_key = ((unsigned long long)x << 32) | (unsigned int)index;
    unsigned long long y_key = ((unsigned long long)y << 32) | (unsigned int)index;
    atomicAdd(&counts[root], 1);
    atomicMin(&min_sum[root], sum_key);
    atomicMax(&max_sum[root], sum_key);
    atomicMin(&min_difference[root], difference_key);
    atomicMax(&max_difference[root], difference_key);
    atomicMin(&min_x[root], x_key);
    atomicMax(&max_x[root], x_key);
    atomicMin(&min_y[root], y_key);
    atomicMax(&max_y[root], y_key);
}
"""


BATCH_BOX_HORIZONTAL_KERNEL = r"""
extern "C" __global__
void batch_box_horizontal(
    const unsigned char* images,
    int width,
    int height,
    int image_stride,
    int image_batch_stride,
    int radius,
    unsigned int* horizontal)
{
    int x = blockDim.x * blockIdx.x + threadIdx.x;
    int y = blockDim.y * blockIdx.y + threadIdx.y;
    int batch = blockIdx.z;
    if (x >= width || y >= height) return;
    const unsigned char* image = images + batch * image_batch_stride;
    unsigned int* output = horizontal + batch * width * height;
    int left = max(0, x - radius);
    int right = min(width - 1, x + radius);
    unsigned int sum = 0;
    for (int source_x = left; source_x <= right; ++source_x) {
        sum += image[y * image_stride + source_x];
    }
    output[y * width + x] = sum;
}
"""


BATCH_THRESHOLD_INIT_KERNEL = r"""
extern "C" __global__
void batch_threshold_and_init_labels(
    const unsigned char* images,
    int width,
    int height,
    int image_stride,
    int image_batch_stride,
    const unsigned int* horizontal,
    int radius,
    int constant,
    unsigned int* labels)
{
    int x = blockDim.x * blockIdx.x + threadIdx.x;
    int y = blockDim.y * blockIdx.y + threadIdx.y;
    int batch = blockIdx.z;
    if (x >= width || y >= height) return;
    int pixel_count = width * height;
    const unsigned char* image = images + batch * image_batch_stride;
    const unsigned int* input_horizontal = horizontal + batch * pixel_count;
    unsigned int* output_labels = labels + batch * pixel_count;
    int top = max(0, y - radius);
    int bottom = min(height - 1, y + radius);
    unsigned int sum = 0;
    for (int source_y = top; source_y <= bottom; ++source_y) {
        sum += input_horizontal[source_y * width + x];
    }
    int horizontal_count = min(width - 1, x + radius) - max(0, x - radius) + 1;
    int vertical_count = bottom - top + 1;
    int count = horizontal_count * vertical_count;
    int index = y * width + x;
    output_labels[index] = ((int)image[y * image_stride + x] + constant) * count < (int)sum
        ? (unsigned int)index + 1
        : 0;
}
"""


BATCH_UNION_KERNEL = r"""
__device__ __forceinline__ unsigned int batch_find_root(
    unsigned int* labels,
    unsigned int label)
{
    while (label != 0) {
        unsigned int parent = labels[label - 1];
        if (parent == label) break;
        label = parent;
    }
    return label;
}

extern "C" __global__
void batch_union_labels(unsigned int* all_labels, int width, int height)
{
    int x = blockDim.x * blockIdx.x + threadIdx.x;
    int y = blockDim.y * blockIdx.y + threadIdx.y;
    int batch = blockIdx.z;
    if (x >= width || y >= height) return;
    int pixel_count = width * height;
    unsigned int* labels = all_labels + batch * pixel_count;
    int index = y * width + x;
    unsigned int own = labels[index];
    if (own == 0) return;
    const int offsets_x[4] = {-1, -1, 0, 1};
    const int offsets_y[4] = {0, -1, -1, -1};
    for (int neighbor_index = 0; neighbor_index < 4; ++neighbor_index) {
        int nx = x + offsets_x[neighbor_index];
        int ny = y + offsets_y[neighbor_index];
        if (nx < 0 || nx >= width || ny < 0) continue;
        unsigned int neighbor = labels[ny * width + nx];
        if (neighbor == 0) continue;
        unsigned int own_root = batch_find_root(labels, own);
        unsigned int neighbor_root = batch_find_root(labels, neighbor);
        while (own_root != neighbor_root) {
            unsigned int high = max(own_root, neighbor_root);
            unsigned int low = min(own_root, neighbor_root);
            unsigned int previous = atomicMin(&labels[high - 1], low);
            if (previous == high || previous == low) break;
            own_root = batch_find_root(labels, low);
            neighbor_root = batch_find_root(labels, previous);
        }
    }
}
"""


BATCH_COMPRESS_KERNEL = r"""
extern "C" __global__
void batch_compress_labels(unsigned int* all_labels, int count)
{
    int index = blockDim.x * blockIdx.x + threadIdx.x;
    int batch = blockIdx.y;
    if (index >= count) return;
    unsigned int* labels = all_labels + batch * count;
    if (labels[index] == 0) return;
    unsigned int root = labels[index];
    while (labels[root - 1] != root) root = labels[root - 1];
    labels[index] = root;
}
"""


BATCH_COMPONENT_STATS_KERNEL = r"""
extern "C" __global__
void batch_component_stats(
    const unsigned int* all_labels,
    int width,
    int height,
    unsigned int* all_counts,
    unsigned long long* all_min_sum,
    unsigned long long* all_max_sum,
    unsigned long long* all_min_difference,
    unsigned long long* all_max_difference,
    unsigned long long* all_min_x,
    unsigned long long* all_max_x,
    unsigned long long* all_min_y,
    unsigned long long* all_max_y)
{
    int index = blockDim.x * blockIdx.x + threadIdx.x;
    int batch = blockIdx.y;
    int pixel_count = width * height;
    if (index >= pixel_count) return;
    const unsigned int* labels = all_labels + batch * pixel_count;
    unsigned int root = labels[index];
    if (root == 0) return;
    int stats_stride = pixel_count + 1;
    unsigned int* counts = all_counts + batch * stats_stride;
    unsigned long long* min_sum = all_min_sum + batch * stats_stride;
    unsigned long long* max_sum = all_max_sum + batch * stats_stride;
    unsigned long long* min_difference = all_min_difference + batch * stats_stride;
    unsigned long long* max_difference = all_max_difference + batch * stats_stride;
    unsigned long long* min_x = all_min_x + batch * stats_stride;
    unsigned long long* max_x = all_max_x + batch * stats_stride;
    unsigned long long* min_y = all_min_y + batch * stats_stride;
    unsigned long long* max_y = all_max_y + batch * stats_stride;
    int x = index % width;
    int y = index / width;
    unsigned int sum_score = (unsigned int)(x + y);
    unsigned int difference_score = (unsigned int)(x - y + height);
    unsigned long long sum_key = ((unsigned long long)sum_score << 32) | (unsigned int)index;
    unsigned long long difference_key =
        ((unsigned long long)difference_score << 32) | (unsigned int)index;
    unsigned long long x_key = ((unsigned long long)x << 32) | (unsigned int)index;
    unsigned long long y_key = ((unsigned long long)y << 32) | (unsigned int)index;
    atomicAdd(&counts[root], 1);
    atomicMin(&min_sum[root], sum_key);
    atomicMax(&max_sum[root], sum_key);
    atomicMin(&min_difference[root], difference_key);
    atomicMax(&max_difference[root], difference_key);
    atomicMin(&min_x[root], x_key);
    atomicMax(&max_x[root], x_key);
    atomicMin(&min_y[root], y_key);
    atomicMax(&max_y[root], y_key);
}
"""


BATCH_FILTER_CANDIDATES_KERNEL = r"""
__device__ __forceinline__ float quadrilateral_area(
    const float* xs,
    const float* ys)
{
    float twice_area = 0.0f;
    #pragma unroll
    for (int corner = 0; corner < 4; ++corner) {
        int next = (corner + 1) & 3;
        twice_area += xs[corner] * ys[next] - xs[next] * ys[corner];
    }
    return 0.5f * fabsf(twice_area);
}

extern "C" __global__
void batch_filter_candidates(
    const unsigned int* all_counts,
    const unsigned long long* all_min_sum,
    const unsigned long long* all_max_sum,
    const unsigned long long* all_min_difference,
    const unsigned long long* all_max_difference,
    const unsigned long long* all_min_x,
    const unsigned long long* all_max_x,
    const unsigned long long* all_min_y,
    const unsigned long long* all_max_y,
    int stats_stride,
    int width,
    unsigned int minimum_component_pixels,
    unsigned int maximum_component_pixels,
    float minimum_quad_area,
    float minimum_compactness,
    float minimum_component_density,
    unsigned int* output_counts,
    float* output_corners)
{
    int root = blockDim.x * blockIdx.x + threadIdx.x;
    int batch = blockIdx.y;
    if (root <= 0 || root >= stats_stride) return;

    int stats_index = batch * stats_stride + root;
    unsigned int component_count = all_counts[stats_index];
    if (component_count < minimum_component_pixels ||
        component_count > maximum_component_pixels) {
        return;
    }

    unsigned int diagonal_indices[4] = {
        (unsigned int)all_min_sum[stats_index],
        (unsigned int)all_max_difference[stats_index],
        (unsigned int)all_max_sum[stats_index],
        (unsigned int)all_min_difference[stats_index],
    };
    unsigned int axis_indices[4] = {
        (unsigned int)all_min_y[stats_index],
        (unsigned int)all_max_x[stats_index],
        (unsigned int)all_max_y[stats_index],
        (unsigned int)all_min_x[stats_index],
    };
    float diagonal_x[4];
    float diagonal_y[4];
    float axis_x[4];
    float axis_y[4];
    #pragma unroll
    for (int corner = 0; corner < 4; ++corner) {
        diagonal_x[corner] = (float)(diagonal_indices[corner] % width);
        diagonal_y[corner] = (float)(diagonal_indices[corner] / width);
        axis_x[corner] = (float)(axis_indices[corner] % width);
        axis_y[corner] = (float)(axis_indices[corner] / width);
    }

    float diagonal_area = quadrilateral_area(diagonal_x, diagonal_y);
    float axis_area = quadrilateral_area(axis_x, axis_y);
    const float* selected_x = axis_area > diagonal_area ? axis_x : diagonal_x;
    const float* selected_y = axis_area > diagonal_area ? axis_y : diagonal_y;
    float area = axis_area > diagonal_area ? axis_area : diagonal_area;
    float perimeter = 0.0f;
    float minimum_side = 3.402823466e+38F;
    #pragma unroll
    for (int corner = 0; corner < 4; ++corner) {
        int next = (corner + 1) & 3;
        float dx = selected_x[next] - selected_x[corner];
        float dy = selected_y[next] - selected_y[corner];
        float side = sqrtf(dx * dx + dy * dy);
        perimeter += side;
        minimum_side = fminf(minimum_side, side);
    }
    float compactness = area / fmaxf(perimeter * perimeter, 1.0f);
    float density = (float)component_count / fmaxf(area, 1.0f);
    if (area < minimum_quad_area ||
        compactness < minimum_compactness ||
        density < minimum_component_density ||
        minimum_side < 3.0f) {
        return;
    }

    unsigned int output_index = atomicAdd(output_counts + batch, 1U);
    size_t output_offset =
        ((size_t)batch * (size_t)stats_stride + output_index) * 8U;
    #pragma unroll
    for (int corner = 0; corner < 4; ++corner) {
        output_corners[output_offset + corner * 2] = selected_x[corner];
        output_corners[output_offset + corner * 2 + 1] = selected_y[corner];
    }
}
"""


@dataclass(frozen=True)
class GpuMarkerObservation:
    marker_id: int
    center_x: float
    center_y: float
    area: float


@dataclass(frozen=True)
class GpuDetectionResult:
    marker_ids: list[int]
    candidate_count: int
    markers: list[GpuMarkerObservation]


class GpuArucoDetector:
    """Detect DICT_4X4_50 IDs without transferring image pixels to the CPU."""

    def __init__(
        self,
        adaptive_window_size: int = 13,
        adaptive_constant: int = 7,
        union_iterations: int = 4,
        minimum_component_pixels: int = 12,
        minimum_quad_area: float = 30.0,
        minimum_compactness: float = 0.035,
        minimum_component_density: float = 0.6,
        maximum_component_area_ratio: float = 0.1,
        allowed_marker_ids: Iterable[int] | None = None,
    ):
        if adaptive_window_size < 3 or adaptive_window_size % 2 == 0:
            raise ValueError("adaptive_window_size must be an odd integer >= 3")
        if maximum_component_area_ratio <= 0 or maximum_component_area_ratio > 1:
            raise ValueError("maximum_component_area_ratio must be in (0, 1]")
        allowed_ids = None if allowed_marker_ids is None else sorted(set(allowed_marker_ids))
        if allowed_ids is not None and any(
            marker_id < 0 or marker_id >= 50 for marker_id in allowed_ids
        ):
            raise ValueError("allowed_marker_ids must contain only DICT_4X4_50 IDs")

        self.decoder = GpuArucoIdDecoder()
        self.cp = self.decoder.cp
        self.radius = adaptive_window_size // 2
        self.adaptive_constant = adaptive_constant
        self.union_iterations = union_iterations
        self.minimum_component_pixels = minimum_component_pixels
        self.minimum_quad_area = minimum_quad_area
        self.minimum_compactness = minimum_compactness
        self.minimum_component_density = minimum_component_density
        self.maximum_component_area_ratio = maximum_component_area_ratio
        self.allowed_ids_device = (
            None if allowed_ids is None else self.cp.asarray(allowed_ids, dtype=self.cp.int16)
        )
        self.box_horizontal_kernel = self.cp.RawKernel(BOX_HORIZONTAL_KERNEL, "box_horizontal")
        self.threshold_kernel = self.cp.RawKernel(
            THRESHOLD_INIT_KERNEL, "threshold_and_init_labels"
        )
        self.union_kernel = self.cp.RawKernel(UNION_KERNEL, "union_labels")
        self.compress_kernel = self.cp.RawKernel(COMPRESS_KERNEL, "compress_labels")
        self.stats_kernel = self.cp.RawKernel(COMPONENT_STATS_KERNEL, "component_stats")
        self.batch_box_horizontal_kernel = self.cp.RawKernel(
            BATCH_BOX_HORIZONTAL_KERNEL, "batch_box_horizontal"
        )
        self.batch_threshold_kernel = self.cp.RawKernel(
            BATCH_THRESHOLD_INIT_KERNEL, "batch_threshold_and_init_labels"
        )
        self.batch_union_kernel = self.cp.RawKernel(BATCH_UNION_KERNEL, "batch_union_labels")
        self.batch_compress_kernel = self.cp.RawKernel(
            BATCH_COMPRESS_KERNEL, "batch_compress_labels"
        )
        self.batch_stats_kernel = self.cp.RawKernel(
            BATCH_COMPONENT_STATS_KERNEL, "batch_component_stats"
        )
        self.batch_filter_candidates_kernel = self.cp.RawKernel(
            BATCH_FILTER_CANDIDATES_KERNEL, "batch_filter_candidates"
        )
        self._batch_workspace_key = None
        self._batch_workspace = None

    def _get_batch_workspace(self, batch_size: int, height: int, width: int):
        if self._batch_workspace_key is not None and self._batch_workspace is not None:
            capacity, workspace_height, workspace_width = self._batch_workspace_key
            if (
                workspace_height == height
                and workspace_width == width
                and capacity >= batch_size
            ):
                return {
                    name: value[:batch_size]
                    for name, value in self._batch_workspace.items()
                }

        cp = self.cp
        capacity = batch_size
        stats_shape = (batch_size, width * height + 1)
        frame_shape = (batch_size, height, width)
        self._batch_workspace_key = (capacity, height, width)
        self._batch_workspace = {
            "horizontal": cp.empty(frame_shape, dtype=cp.uint32),
            "labels": cp.empty(frame_shape, dtype=cp.uint32),
            "counts": cp.empty(stats_shape, dtype=cp.uint32),
            "min_sum": cp.empty(stats_shape, dtype=cp.uint64),
            "max_sum": cp.empty(stats_shape, dtype=cp.uint64),
            "min_difference": cp.empty(stats_shape, dtype=cp.uint64),
            "max_difference": cp.empty(stats_shape, dtype=cp.uint64),
            "min_x": cp.empty(stats_shape, dtype=cp.uint64),
            "max_x": cp.empty(stats_shape, dtype=cp.uint64),
            "min_y": cp.empty(stats_shape, dtype=cp.uint64),
            "max_y": cp.empty(stats_shape, dtype=cp.uint64),
            "candidate_counts": cp.empty(batch_size, dtype=cp.uint32),
            "candidate_corners": cp.empty(
                (batch_size, width * height + 1, 4, 2), dtype=cp.float32
            ),
        }
        return {
            name: value[:batch_size]
            for name, value in self._batch_workspace.items()
        }

    def _extract_candidate_corners_batch(
        self,
        gray_batch,
        profile_events=None,
        host_timings=None,
    ):
        cp = self.cp
        stage_started = None
        if profile_events is not None:
            stage_started = cp.cuda.Event()
            stage_started.record()

        def finish_stage(name: str) -> None:
            nonlocal stage_started
            if profile_events is None:
                return
            stage_finished = cp.cuda.Event()
            stage_finished.record()
            profile_events.append((name, stage_started, stage_finished))
            stage_started = stage_finished

        if gray_batch.ndim != 3:
            raise ValueError("gray_batch must have shape (batch, height, width)")
        if not gray_batch.flags.c_contiguous:
            gray_batch = cp.ascontiguousarray(gray_batch)
        batch_size, height, width = gray_batch.shape
        if batch_size < 1:
            return (
                cp.empty((0, 4, 2), dtype=cp.float32),
                np.empty(0, dtype=np.int64),
            )
        pixel_count = width * height
        block_2d = (16, 16)
        grid_2d = (
            (width + block_2d[0] - 1) // block_2d[0],
            (height + block_2d[1] - 1) // block_2d[1],
            batch_size,
        )
        workspace = self._get_batch_workspace(batch_size, height, width)
        horizontal = workspace["horizontal"]
        labels = workspace["labels"]
        self.batch_box_horizontal_kernel(
            grid_2d,
            block_2d,
            (
                gray_batch,
                np.int32(width),
                np.int32(height),
                np.int32(gray_batch.strides[1]),
                np.int32(gray_batch.strides[0]),
                np.int32(self.radius),
                horizontal,
            ),
        )
        self.batch_threshold_kernel(
            grid_2d,
            block_2d,
            (
                gray_batch,
                np.int32(width),
                np.int32(height),
                np.int32(gray_batch.strides[1]),
                np.int32(gray_batch.strides[0]),
                horizontal,
                np.int32(self.radius),
                np.int32(self.adaptive_constant),
                labels,
            ),
        )
        finish_stage("candidatePreprocessGpuMs")

        block_1d = (256,)
        grid_1d = ((pixel_count + block_1d[0] - 1) // block_1d[0], batch_size)
        for _ in range(self.union_iterations):
            self.batch_union_kernel(
                grid_2d, block_2d, (labels, np.int32(width), np.int32(height))
            )
            self.batch_compress_kernel(
                grid_1d, block_1d, (labels, np.int32(pixel_count))
            )
        self.batch_compress_kernel(grid_1d, block_1d, (labels, np.int32(pixel_count)))
        finish_stage("candidateUnionGpuMs")

        counts = workspace["counts"]
        minimum_key = np.iinfo(np.uint64).max
        min_sum = workspace["min_sum"]
        max_sum = workspace["max_sum"]
        min_difference = workspace["min_difference"]
        max_difference = workspace["max_difference"]
        min_x = workspace["min_x"]
        max_x = workspace["max_x"]
        min_y = workspace["min_y"]
        max_y = workspace["max_y"]
        counts.fill(0)
        min_sum.fill(minimum_key)
        max_sum.fill(0)
        min_difference.fill(minimum_key)
        max_difference.fill(0)
        min_x.fill(minimum_key)
        max_x.fill(0)
        min_y.fill(minimum_key)
        max_y.fill(0)
        finish_stage("candidateWorkspaceResetGpuMs")
        self.batch_stats_kernel(
            grid_1d,
            block_1d,
            (
                labels,
                np.int32(width),
                np.int32(height),
                counts,
                min_sum,
                max_sum,
                min_difference,
                max_difference,
                min_x,
                max_x,
                min_y,
                max_y,
            ),
        )
        finish_stage("candidateStatsGpuMs")

        candidate_counts_device = workspace["candidate_counts"]
        candidate_corners = workspace["candidate_corners"]
        candidate_counts_device.fill(0)
        maximum_component_pixels = max(
            self.minimum_component_pixels,
            int(pixel_count * self.maximum_component_area_ratio),
        )
        filter_grid = (
            (pixel_count + 1 + block_1d[0] - 1) // block_1d[0],
            batch_size,
        )
        self.batch_filter_candidates_kernel(
            filter_grid,
            block_1d,
            (
                counts,
                min_sum,
                max_sum,
                min_difference,
                max_difference,
                min_x,
                max_x,
                min_y,
                max_y,
                np.int32(pixel_count + 1),
                np.int32(width),
                np.uint32(self.minimum_component_pixels),
                np.uint32(maximum_component_pixels),
                np.float32(self.minimum_quad_area),
                np.float32(self.minimum_compactness),
                np.float32(self.minimum_component_density),
                candidate_counts_device,
                candidate_corners,
            ),
        )
        finish_stage("candidateFilterGpuMs")

        if profile_events is not None:
            cp.cuda.get_current_stream().synchronize()
        count_copy_started = time.perf_counter()
        candidate_counts = cp.asnumpy(candidate_counts_device).astype(np.int64)
        if host_timings is not None:
            host_timings["candidateCountD2hMs"] = (
                time.perf_counter() - count_copy_started
            ) * 1000.0
        compact_started = time.perf_counter()
        corner_parts = []
        for source_index, candidate_count in enumerate(candidate_counts):
            if candidate_count <= 0:
                continue
            corner_parts.append(candidate_corners[source_index, :candidate_count])
        if corner_parts:
            corners = cp.concatenate(corner_parts, axis=0)
        else:
            corners = cp.empty((0, 4, 2), dtype=cp.float32)
        if host_timings is not None:
            host_timings["candidateCompactWallMs"] = (
                time.perf_counter() - compact_started
            ) * 1000.0
        return corners, candidate_counts

    def _extract_candidate_corners(self, gray_device):
        cp = self.cp
        height, width = gray_device.shape
        pixel_count = width * height
        block_2d = (16, 16)
        grid_2d = (
            (width + block_2d[0] - 1) // block_2d[0],
            (height + block_2d[1] - 1) // block_2d[1],
        )
        horizontal = cp.empty((height, width), dtype=cp.uint32)
        labels = cp.empty((height, width), dtype=cp.uint32)
        self.box_horizontal_kernel(
            grid_2d,
            block_2d,
            (
                gray_device,
                np.int32(width),
                np.int32(height),
                np.int32(gray_device.strides[0]),
                np.int32(self.radius),
                horizontal,
            ),
        )
        self.threshold_kernel(
            grid_2d,
            block_2d,
            (
                gray_device,
                np.int32(width),
                np.int32(height),
                np.int32(gray_device.strides[0]),
                horizontal,
                np.int32(self.radius),
                np.int32(self.adaptive_constant),
                labels,
            ),
        )

        block_1d = (256,)
        grid_1d = ((pixel_count + block_1d[0] - 1) // block_1d[0],)
        for _ in range(self.union_iterations):
            self.union_kernel(grid_2d, block_2d, (labels, np.int32(width), np.int32(height)))
            self.compress_kernel(grid_1d, block_1d, (labels, np.int32(pixel_count)))
        self.compress_kernel(grid_1d, block_1d, (labels, np.int32(pixel_count)))

        counts = cp.zeros(pixel_count + 1, dtype=cp.uint32)
        minimum_key = np.iinfo(np.uint64).max
        min_sum = cp.full(pixel_count + 1, minimum_key, dtype=cp.uint64)
        max_sum = cp.zeros(pixel_count + 1, dtype=cp.uint64)
        min_difference = cp.full(pixel_count + 1, minimum_key, dtype=cp.uint64)
        max_difference = cp.zeros(pixel_count + 1, dtype=cp.uint64)
        min_x = cp.full(pixel_count + 1, minimum_key, dtype=cp.uint64)
        max_x = cp.zeros(pixel_count + 1, dtype=cp.uint64)
        min_y = cp.full(pixel_count + 1, minimum_key, dtype=cp.uint64)
        max_y = cp.zeros(pixel_count + 1, dtype=cp.uint64)
        self.stats_kernel(
            grid_1d,
            block_1d,
            (
                labels,
                np.int32(width),
                np.int32(height),
                counts,
                min_sum,
                max_sum,
                min_difference,
                max_difference,
                min_x,
                max_x,
                min_y,
                max_y,
            ),
        )

        roots = cp.nonzero(counts >= self.minimum_component_pixels)[0]
        roots = roots[roots > 0]
        if len(roots) == 0:
            return cp.empty((0, 4, 2), dtype=cp.float32), cp.empty(0, dtype=cp.float32)

        diagonal_keys = cp.stack(
            (min_sum[roots], max_difference[roots], max_sum[roots], min_difference[roots]),
            axis=1,
        )
        axis_keys = cp.stack(
            (min_y[roots], max_x[roots], max_y[roots], min_x[roots]),
            axis=1,
        )
        diagonal_indices = (diagonal_keys & cp.uint64(0xFFFFFFFF)).astype(cp.int32)
        axis_indices = (axis_keys & cp.uint64(0xFFFFFFFF)).astype(cp.int32)
        diagonal_corners = cp.stack(
            (diagonal_indices % width, diagonal_indices // width), axis=2
        ).astype(cp.float32)
        axis_corners = cp.stack((axis_indices % width, axis_indices // width), axis=2).astype(
            cp.float32
        )
        diagonal_shifted = cp.roll(diagonal_corners, -1, axis=1)
        axis_shifted = cp.roll(axis_corners, -1, axis=1)
        diagonal_area = 0.5 * cp.abs(
            cp.sum(
                diagonal_corners[:, :, 0] * diagonal_shifted[:, :, 1]
                - diagonal_shifted[:, :, 0] * diagonal_corners[:, :, 1],
                axis=1,
            )
        )
        axis_area = 0.5 * cp.abs(
            cp.sum(
                axis_corners[:, :, 0] * axis_shifted[:, :, 1]
                - axis_shifted[:, :, 0] * axis_corners[:, :, 1],
                axis=1,
            )
        )
        corners = cp.where((axis_area > diagonal_area)[:, None, None], axis_corners, diagonal_corners)
        shifted = cp.roll(corners, -1, axis=1)
        side_lengths = cp.sqrt(cp.sum((shifted - corners) ** 2, axis=2))
        area = 0.5 * cp.abs(
            cp.sum(corners[:, :, 0] * shifted[:, :, 1] - shifted[:, :, 0] * corners[:, :, 1], axis=1)
        )
        perimeter = cp.sum(side_lengths, axis=1)
        compactness = area / cp.maximum(perimeter * perimeter, cp.float32(1.0))
        component_density = counts[roots] / cp.maximum(area, cp.float32(1.0))
        maximum_component_pixels = max(
            self.minimum_component_pixels,
            int(pixel_count * self.maximum_component_area_ratio),
        )
        valid = (
            (counts[roots] <= maximum_component_pixels)
            & (area >= self.minimum_quad_area)
            & (compactness >= self.minimum_compactness)
            & (component_density >= self.minimum_component_density)
            & (cp.min(side_lengths, axis=1) >= 3.0)
        )
        return corners[valid], component_density[valid]

    def _homographies_from_corners(self, corners):
        cp = self.cp
        if len(corners) == 0:
            return cp.empty((0, 3, 3), dtype=cp.float32)
        x0, y0 = corners[:, 0, 0], corners[:, 0, 1]
        x1, y1 = corners[:, 1, 0], corners[:, 1, 1]
        x2, y2 = corners[:, 2, 0], corners[:, 2, 1]
        x3, y3 = corners[:, 3, 0], corners[:, 3, 1]
        dx1, dx2 = x1 - x2, x3 - x2
        dy1, dy2 = y1 - y2, y3 - y2
        sx, sy = x0 - x1 + x2 - x3, y0 - y1 + y2 - y3
        denominator = dx1 * dy2 - dx2 * dy1
        safe_denominator = cp.where(
            cp.abs(denominator) < 1e-6, cp.float32(1e-6), denominator
        )
        g = (sx * dy2 - dx2 * sy) / safe_denominator
        h = (dx1 * sy - sx * dy1) / safe_denominator
        scale = cp.float32(PATCH_SIZE - 1)
        homographies = cp.empty((len(corners), 3, 3), dtype=cp.float32)
        homographies[:, 0, 0] = (x1 - x0 + g * x1) / scale
        homographies[:, 0, 1] = (x3 - x0 + h * x3) / scale
        homographies[:, 0, 2] = x0
        homographies[:, 1, 0] = (y1 - y0 + g * y1) / scale
        homographies[:, 1, 1] = (y3 - y0 + h * y3) / scale
        homographies[:, 1, 2] = y0
        homographies[:, 2, 0] = g / scale
        homographies[:, 2, 1] = h / scale
        homographies[:, 2, 2] = 1.0
        return homographies

    def detect(self, gray_device) -> GpuDetectionResult:
        corners, _ = self._extract_candidate_corners(gray_device)
        homographies = self._homographies_from_corners(corners)
        ids, _, _, _ = self.decoder.decode_device(gray_device, homographies)
        valid = ids >= 0
        if self.allowed_ids_device is not None:
            valid &= self.cp.isin(ids, self.allowed_ids_device)
        valid_ids = ids[valid]
        valid_corners = corners[valid]
        height, width = gray_device.shape
        markers = self._build_marker_observations(
            self.cp.asnumpy(valid_ids),
            self.cp.asnumpy(valid_corners),
            width,
            height,
        )
        return GpuDetectionResult(
            marker_ids=[marker.marker_id for marker in markers],
            candidate_count=len(corners),
            markers=markers,
        )

    def detect_batch(
        self,
        gray_batch,
        timings: dict[str, float] | None = None,
    ) -> list[GpuDetectionResult]:
        cp = self.cp
        profiling = timings is not None
        input_started = time.perf_counter() if profiling else 0.0
        gray_batch = cp.asarray(gray_batch, dtype=cp.uint8)
        if profiling:
            timings["detectorInputWallMs"] = (
                time.perf_counter() - input_started
            ) * 1000.0
        if gray_batch.ndim != 3 or gray_batch.shape[0] < 1:
            raise ValueError("gray_batch must have shape (batch, height, width)")
        batch_size = int(gray_batch.shape[0])

        candidate_started = cp.cuda.Event() if profiling else None
        candidate_finished = cp.cuda.Event() if profiling else None
        if profiling:
            candidate_started.record()
        candidate_profile_events = [] if profiling else None
        candidate_host_timings: dict[str, float] | None = {} if profiling else None
        corners, candidate_counts = self._extract_candidate_corners_batch(
            gray_batch,
            candidate_profile_events,
            candidate_host_timings,
        )
        candidate_gpu_ms = 0.0
        d2h_ms = 0.0
        if profiling:
            candidate_finished.record()
            candidate_finished.synchronize()
            candidate_gpu_ms = cp.cuda.get_elapsed_time(
                candidate_started,
                candidate_finished,
            )
            for name, started, finished in candidate_profile_events:
                timings[name] = cp.cuda.get_elapsed_time(started, finished)
            timings.update(candidate_host_timings)
            d2h_ms = candidate_host_timings["candidateCountD2hMs"]
        if len(corners) == 0:
            if profiling:
                timings["candidateGpuMs"] = candidate_gpu_ms
                timings["decodeGpuMs"] = 0.0
                timings["gpuKernelMs"] = candidate_gpu_ms
                timings["d2hMs"] = d2h_ms
                timings["resultFormatMs"] = 0.0
            return [
                GpuDetectionResult(marker_ids=[], candidate_count=0, markers=[])
                for _ in range(batch_size)
            ]

        decode_started = cp.cuda.Event() if profiling else None
        decode_finished = cp.cuda.Event() if profiling else None
        if profiling:
            decode_started.record()
        homographies = self._homographies_from_corners(corners)
        decoded_parts = []
        offset = 0
        for source_index, candidate_count in enumerate(candidate_counts):
            next_offset = offset + int(candidate_count)
            if next_offset > offset:
                ids, _, _, _ = self.decoder.decode_device(
                    gray_batch[source_index], homographies[offset:next_offset]
                )
                decoded_parts.append(ids)
            offset = next_offset
        all_ids = cp.concatenate(decoded_parts) if decoded_parts else cp.empty(0, dtype=cp.int16)
        valid = all_ids >= 0
        if self.allowed_ids_device is not None:
            valid &= cp.isin(all_ids, self.allowed_ids_device)
        decode_gpu_ms = 0.0
        if profiling:
            decode_finished.record()
            decode_finished.synchronize()
            decode_gpu_ms = cp.cuda.get_elapsed_time(decode_started, decode_finished)

        result_copy_started = time.perf_counter() if profiling else 0.0
        ids_host = cp.asnumpy(all_ids)
        valid_host = cp.asnumpy(valid)
        corners_host = cp.asnumpy(corners)
        if profiling:
            d2h_ms += (time.perf_counter() - result_copy_started) * 1000.0
        height = int(gray_batch.shape[1])
        width = int(gray_batch.shape[2])

        format_started = time.perf_counter() if profiling else 0.0
        results = []
        offset = 0
        for candidate_count in candidate_counts:
            next_offset = offset + int(candidate_count)
            accepted = valid_host[offset:next_offset]
            markers = self._build_marker_observations(
                ids_host[offset:next_offset][accepted],
                corners_host[offset:next_offset][accepted],
                width,
                height,
            )
            results.append(
                GpuDetectionResult(
                    marker_ids=[marker.marker_id for marker in markers],
                    candidate_count=int(candidate_count),
                    markers=markers,
                )
            )
            offset = next_offset
        if profiling:
            timings["candidateGpuMs"] = candidate_gpu_ms
            timings["decodeGpuMs"] = decode_gpu_ms
            timings["gpuKernelMs"] = candidate_gpu_ms + decode_gpu_ms
            timings["d2hMs"] = d2h_ms
            timings["resultFormatMs"] = (
                time.perf_counter() - format_started
            ) * 1000.0
        return results

    @staticmethod
    def _build_marker_observations(ids_host, corners_host, width, height):
        observations = []
        width_scale = max(1.0, float(width))
        height_scale = max(1.0, float(height))
        image_area = width_scale * height_scale
        for marker_id, corners in zip(ids_host, corners_host):
            x = corners[:, 0]
            y = corners[:, 1]
            shifted_x = np.roll(x, -1)
            shifted_y = np.roll(y, -1)
            area = 0.5 * abs(float(np.sum(x * shifted_y - shifted_x * y)))
            observations.append(
                GpuMarkerObservation(
                    marker_id=int(marker_id),
                    center_x=float(np.mean(x) / width_scale),
                    center_y=float(np.mean(y) / height_scale),
                    area=float(area / image_area),
                )
            )
        return observations
