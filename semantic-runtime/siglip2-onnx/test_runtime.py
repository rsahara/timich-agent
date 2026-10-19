"""Exercise real preprocessing without downloading model weights."""

from io import BytesIO
import unittest

import numpy as np
from PIL import Image
from transformers import SiglipImageProcessor

from server import SigLIP2ONNXRuntime


class RecordingSession:
    def run(self, outputs, inputs):
        pixels = inputs["pixel_values"]
        if pixels.shape != (1, 3, 224, 224) or not np.isfinite(pixels).all():
            raise AssertionError(f"invalid preprocessed RGB image: {pixels.shape}")
        return [np.array([[1.0, 0.0]], dtype=np.float32)]


class ImagePreprocessingTests(unittest.TestCase):
    def test_rgb_channel_axis_for_small_and_regular_images(self):
        runtime = object.__new__(SigLIP2ONNXRuntime)
        runtime.processor = SiglipImageProcessor(size={"height": 224, "width": 224})
        runtime.image_session = RecordingSession()
        runtime.expected_dim = 2
        for size in ((1, 1), (3, 7), (224, 224)):
            with self.subTest(size=size):
                data = BytesIO()
                Image.new("RGB", size, (128, 64, 32)).save(data, format="PNG")
                self.assertEqual(runtime.embed_image(data.getvalue()), [1.0, 0.0])


if __name__ == "__main__":
    unittest.main()
