// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

export interface ReactiveProbe {
  readonly runs: number;
  dispose(): void;
}

export function createReactiveProbe(readFn: () => void): ReactiveProbe {
  let runs = 0;
  const dispose = $effect.root(() => {
    $effect(() => {
      readFn();
      runs += 1;
    });
  });
  return {
    get runs() {
      return runs;
    },
    dispose,
  };
}
