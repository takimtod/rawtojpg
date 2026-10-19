# syntax=docker/dockerfile:1
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
	PYTHONUNBUFFERED=1

RUN apt-get update \
	&& apt-get install -y --no-install-recommends libgomp1 \
	&& rm -rf /var/lib/apt/lists/* \
	&& pip install --no-cache-dir rawpy Pillow

WORKDIR /app

RUN <<'EOF'
cat > /app/convert.py <<'PY'
import os
import rawpy
from PIL import Image

WORKSPACE = "/github/workspace"
RAW_EXTENSIONS = {
	".3fr", ".arw", ".cr2", ".cr3", ".dng", ".erf", ".fff", ".iiq",
	".kdc", ".mef", ".mos", ".mrw", ".nef", ".nrw", ".orf", ".pef",
	".raf", ".raw", ".rw2", ".rwl", ".sr2", ".srf", ".srw",
}

converted = 0
for root, _, files in os.walk(WORKSPACE):
	for filename in files:
		source = os.path.join(root, filename)
		if os.path.splitext(filename)[1].lower() not in RAW_EXTENSIONS:
			continue

		target = os.path.splitext(source)[0] + ".jpg"
		with rawpy.imread(source) as raw:
			pixels = raw.postprocess(use_camera_wb=True)
		Image.fromarray(pixels).save(target, "JPEG", quality=95)
		print(f"Converted {source} -> {target}")
		converted += 1

print(f"Converted {converted} RAW file(s)")
PY
EOF

ENTRYPOINT ["python", "/app/convert.py"]
