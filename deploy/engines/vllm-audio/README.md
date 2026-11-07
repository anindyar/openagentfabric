# vllm-audio: vLLM with its audio extra, for speech-to-text

The catalog's Whisper entry (`whisper-large-v3-turbo`) runs on this engine. It is the pinned upstream
vLLM image (`vllm` in `bundle/versions.yaml`) with the four packages of vLLM's `audio` extra that the
published image leaves out (av, soundfile, soxr, scipy). vLLM then serves `/v1/audio/transcriptions`
and `/v1/audio/translations` for Whisper on the GPU, behind the gateway like every other model.

It is a local component (`local: true`): there is no published build, so it is built on the box and
pushed to the control node's registry, which pins it in `cluster.yaml` (`spec.images`):

```sh
docker build -t vllm-audio:0.31.0 deploy/engines/vllm-audio
sudo oaf images push vllm-audio:0.31.0 --component vllm-audio
sudo oaf apply
```

Until it is pushed, a deployment that uses it is not started and `oaf apply` says what to push; the
rest of the cluster applies as usual.

Licences: vLLM (Apache-2.0) and the NVIDIA CUDA Toolkit EULA, as for the upstream image (accept
`image/vllm-audio`); av (BSD-3-Clause, bundles FFmpeg under LGPL-2.1), soundfile (BSD-3-Clause,
bundles libsndfile under LGPL-2.1), soxr (LGPL-2.1), scipy (BSD-3-Clause). The LGPL libraries are
dynamically linked shared objects inside the wheels, unmodified.

Status: not yet run on a GB10. The build checks that the packages import; the acceptance test on the
box is a transcription through the gateway (docs/user-guide.md, Speech).
