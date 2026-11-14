import { useCallback, useEffect, useState } from 'react';
import { errorText } from './api';

export function useData<T>(load: () => Promise<T>, revision = 0) {
  const [attempt, setAttempt] = useState(0);
  const [state, setState] = useState<{
    data?: T;
    error: string;
    token: string;
  }>({ error: '', token: '' });
  const token = `${revision}:${attempt}`;
  const refresh = useCallback(() => setAttempt((value) => value + 1), []);
  useEffect(() => {
    let current = true;
    load()
      .then((value) => {
        if (current) setState({ data: value, error: '', token });
      })
      .catch((reason) => {
        if (current)
          setState((previous) => ({
            ...previous,
            error: errorText(reason),
            token,
          }));
      });
    return () => {
      current = false;
    };
  }, [load, token]);
  return {
    data: state.data,
    error: state.token === token ? state.error : '',
    loading: state.token !== token,
    refresh,
  };
}
