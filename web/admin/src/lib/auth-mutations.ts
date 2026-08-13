import { mutationOptions } from "@tanstack/react-query"

import type {
  TwoFactorChallenge,
  TwoFactorVerificationMode,
} from "@/lib/auth"

export const authMutations = {
  // Resolves to a challenge when the account needs a second factor, else null.
  login: (
    loginWithPassword: (
      login: string,
      password: string
    ) => Promise<TwoFactorChallenge | null>
  ) =>
    mutationOptions({
      mutationKey: ["auth", "login"],
      mutationFn: ({ login, password }: { login: string; password: string }) =>
        loginWithPassword(login, password),
    }),
  verifyTwoFactor: (
    completeTwoFactor: (
      challenge: TwoFactorChallenge,
      code: string,
      mode: TwoFactorVerificationMode
    ) => Promise<void>
  ) =>
    mutationOptions({
      mutationKey: ["auth", "2fa", "verify"],
      mutationFn: ({
        challenge,
        code,
        mode,
      }: {
        challenge: TwoFactorChallenge
        code: string
        mode: TwoFactorVerificationMode
      }) => completeTwoFactor(challenge, code, mode),
    }),
  selectTwoFactor: (
    selectTwoFactor: (
      challenge: TwoFactorChallenge,
      factorId: string
    ) => Promise<TwoFactorChallenge>
  ) =>
    mutationOptions({
      mutationKey: ["auth", "2fa", "challenge"],
      mutationFn: ({
        challenge,
        factorId,
      }: {
        challenge: TwoFactorChallenge
        factorId: string
      }) => selectTwoFactor(challenge, factorId),
    }),
  logout: (logout: () => Promise<void>) =>
    mutationOptions({
      mutationKey: ["auth", "logout"],
      mutationFn: logout,
    }),
}
