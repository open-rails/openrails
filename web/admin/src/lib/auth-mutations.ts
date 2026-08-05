import { mutationOptions } from "@tanstack/react-query"

export const authMutations = {
  login: (
    loginWithPassword: (login: string, password: string) => Promise<void>
  ) =>
    mutationOptions({
      mutationKey: ["auth", "login"],
      mutationFn: ({ login, password }: { login: string; password: string }) =>
        loginWithPassword(login, password),
    }),
  logout: (logout: () => Promise<void>) =>
    mutationOptions({
      mutationKey: ["auth", "logout"],
      mutationFn: logout,
    }),
}
