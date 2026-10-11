"""At most three free transport observations; never repeat a financial send."""
from pathlib import Path
import time

import decision_window_metadata as original
from batch_acquisition_proof import file_hash

ORIGINAL_SHA = 'c4060de4aad9fdbef2ac924ac28ff68b6dbb15e78999de23555de47004c41032'


def fetch(query, directory, **kwargs):
    if file_hash(Path(original.__file__)) != ORIGINAL_SHA:
        raise ValueError('original fixed-endpoint metadata code differs')
    attempts = []
    for index in range(3):
        target = directory / f'transport-observation-{index:03d}';target.mkdir(mode=0o700)
        try:
            value = original.fetch(query, target, **kwargs)
        except ValueError as error:
            if str(error) != 'free metadata transport stopped':
                raise
            attempts.append(dict(directory=str(target), completed=False, error_class='transport', financial_requests=0))
            if index == 2:
                original.save(directory / 'transport-failure.json', dict(attempts=attempts,
                    maximum_observation_attempts=3, financial_requests=0, financial_retry=False))
                raise ValueError('free metadata transport exhausted; no financial request or retry') from None
            time.sleep(2 * (index + 1));continue
        attempts.append(dict(directory=str(target), completed=True, receipt_sha256=file_hash(target / 'receipt.json'),
                             financial_requests=0))
        value = dict(value, metadata_observation_attempts=index + 1,
            readonly_transport_retries=index, financial_retry=False)
        original.save(directory / 'transport-observations.json', dict(attempts=attempts,
            maximum_observation_attempts=3, financial_requests=0, financial_retry=False))
        original.save(directory / 'receipt.json', value)
        return value
