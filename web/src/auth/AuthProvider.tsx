import { ClerkProvider, SignInButton, UserButton } from '@clerk/react'
import type { ReactNode } from 'react'
import { CLERK_KEY } from './config'

export function AuthProvider({ children }: { children: ReactNode }) {
  if (!CLERK_KEY) return <>{children}</>
  return <ClerkProvider publishableKey={CLERK_KEY}>{children}</ClerkProvider>
}

export function SignInControl() {
  return (
    <SignInButton mode="modal">
      <button type="button" className="button">
        Sign in
      </button>
    </SignInButton>
  )
}

export function AccountButton() {
  return <UserButton />
}
