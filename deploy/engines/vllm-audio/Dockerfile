# vllm-audio: the pinned upstream vLLM image plus vLLM's `audio` extra, so it
# serves Whisper on /v1/audio/transcriptions and /v1/audio/translations.
#
# vLLM's published image installs the vLLM wheel without its extras, and the
# speech-to-text endpoints need av, soundfile, soxr and scipy to decode audio.
# Nothing else changes: same vLLM, same CUDA, same licences.
#
# The base is the `vllm` component of bundle/versions.yaml, by digest; bump it
# together with that pin. The extras are pinned to versions that publish
# cp312 aarch64 wheels (the base is Python 3.12).
FROM docker.io/vllm/vllm-openai:v0.31.0@sha256:c1c9f6fd5c109ba7f0546a59f5b2f15fb87f64c77782e90a27b648b42a8e67c3

RUN uv pip install --system --no-cache "av==19.0.2" "soundfile==0.14.0" "soxr==1.1.0" "scipy==1.18.1" \
    && python3 -c "import av, soundfile, soxr, scipy.signal; print('audio extras ok')"
