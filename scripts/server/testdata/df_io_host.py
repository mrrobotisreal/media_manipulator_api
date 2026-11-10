from dataclasses import dataclass
from pathlib import Path
from typing import Optional, Tuple, Union

import numpy as np
import soundfile as sf
import torch
import torchaudio.functional as taf
from loguru import logger


@dataclass
class AudioMetaData:
    sample_rate: int
    num_frames: int
    num_channels: int
    bits_per_sample: int = 0
    encoding: str = "UNKNOWN"


def _metadata(path: Union[str, Path]) -> AudioMetaData:
    info = sf.info(str(path))
    return AudioMetaData(
        sample_rate=int(info.samplerate),
        num_frames=int(info.frames),
        num_channels=int(info.channels),
        bits_per_sample=0,
        encoding=str(info.subtype or "UNKNOWN"),
    )


def _to_channels_first(audio_np: np.ndarray) -> torch.Tensor:
    # soundfile returns mono as [frames], stereo/multichannel as [frames, channels].
    if audio_np.ndim == 1:
        audio_np = audio_np[:, None]
    audio_np = np.ascontiguousarray(audio_np.T)
    return torch.from_numpy(audio_np)


def _to_soundfile_shape(audio: torch.Tensor) -> np.ndarray:
    # DeepFilterNet uses [channels, frames]. soundfile wants [frames, channels].
    audio = audio.detach().cpu()
    if audio.ndim == 1:
        audio = audio.unsqueeze(0)
    return audio.transpose(0, 1).contiguous().numpy()


def resample(
    audio: torch.Tensor,
    orig_sr: int,
    new_sr: int,
    method: str = "sinc_interp_hann",
    device: Optional[Union[str, torch.device]] = None,
) -> torch.Tensor:
    if int(orig_sr) == int(new_sr):
        return audio

    original_device = audio.device
    if device is not None:
        audio = audio.to(device)

    audio = taf.resample(
        audio,
        orig_freq=int(orig_sr),
        new_freq=int(new_sr),
        resampling_method=method,
    )

    if device is not None:
        audio = audio.to(original_device)

    return audio


def load_audio(
    file: Union[str, Path],
    sr: Optional[int] = None,
    device: Union[str, torch.device] = "cpu",
    dtype: torch.dtype = torch.float32,
) -> Tuple[torch.Tensor, AudioMetaData]:
    file = Path(file)
    if not file.exists():
        raise FileNotFoundError(file)

    meta = _metadata(file)

    audio_np, audio_sr = sf.read(str(file), dtype="float32", always_2d=True)
    audio = _to_channels_first(audio_np).to(dtype=dtype)

    if sr is not None and int(audio_sr) != int(sr):
        audio = resample(audio, int(audio_sr), int(sr), device=device)
        meta = AudioMetaData(
            sample_rate=int(sr),
            num_frames=int(audio.shape[-1]),
            num_channels=int(audio.shape[0]),
            bits_per_sample=meta.bits_per_sample,
            encoding=meta.encoding,
        )

    audio = audio.to(device)
    return audio, meta


def save_audio(
    file: Union[str, Path],
    audio: torch.Tensor,
    sr: int,
    subtype: Optional[str] = None,
    **kwargs,
) -> None:
    file = Path(file)
    file.parent.mkdir(parents=True, exist_ok=True)

    data = _to_soundfile_shape(audio)

    # Prefer 16-bit PCM for WAV unless caller explicitly passes a subtype.
    if subtype is None and file.suffix.lower() == ".wav":
        subtype = "PCM_16"

    sf.write(str(file), data, int(sr), subtype=subtype)
    logger.debug(f"Saved audio to {file}")

