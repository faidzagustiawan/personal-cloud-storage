import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query";

import { ApiError, api } from "../lib/api";
import type { Folder, Me, StorageUsage } from "../lib/types";

export const keys = {
  me: ["me"] as const,
  usage: ["usage"] as const,
  folders: ["folders"] as const,
  files: (scope: Record<string, unknown>) => ["files", scope] as const,
};

/**
 * The current session, or null when signed out.
 *
 * A 401 is a normal answer here, not a failure — it is how the server says
 * "nobody is signed in". Mapping it to null keeps it out of the error path, so
 * the router can redirect quietly instead of an error boundary firing on every
 * cold load.
 */
export function useMe(): UseQueryResult<Me | null, Error> {
  return useQuery({
    queryKey: keys.me,
    queryFn: async () => {
      try {
        return await api.me();
      } catch (error) {
        if (error instanceof ApiError && error.isUnauthenticated) return null;
        throw error;
      }
    },
    staleTime: 60_000,
    retry: false,
  });
}

export function useLogin() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ username, password }: { username: string; password: string }) =>
      api.login(username, password),
    onSuccess: (me) => {
      client.setQueryData(keys.me, me);
      client.invalidateQueries({ queryKey: keys.usage });
    },
  });
}

export function useLogout() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => api.logout(),
    // Clear everything on the way out, whichever way it went: leaving another
    // account's file list in the cache after a sign-out would show it to
    // whoever signs in next on this device.
    onSettled: () => {
      client.setQueryData(keys.me, null);
      client.clear();
    },
  });
}

export function useChangePassword() {
  return useMutation({
    mutationFn: ({ current, next }: { current: string; next: string }) =>
      api.changePassword(current, next),
  });
}

export function useUsage(enabled = true): UseQueryResult<StorageUsage, Error> {
  return useQuery({
    queryKey: keys.usage,
    queryFn: () => api.usage(),
    enabled,
    staleTime: 30_000,
  });
}

export function useFolders(enabled = true): UseQueryResult<Folder[], Error> {
  return useQuery({
    queryKey: keys.folders,
    queryFn: async () => (await api.listFolders()).items ?? [],
    enabled,
    staleTime: 60_000,
  });
}

export function useCreateFolder() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.createFolder(name),
    onSuccess: () => client.invalidateQueries({ queryKey: keys.folders }),
  });
}
