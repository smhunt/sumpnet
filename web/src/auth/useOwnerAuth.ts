import { useAuth } from '@clerk/react'
import { useCallback } from 'react'
import type { TokenGetter } from '../lib/api'
import { CLERK_KEY } from './config'

export interface OwnerAuth {
  configured: boolean
  loaded: boolean
  signedIn: boolean
  identity: string // Clerk user id, or "anonymous"
  getToken: TokenGetter
}

const noToken: TokenGetter = async () => null
const publicOnly: OwnerAuth = { configured: false, loaded: true, signedIn: false, identity: 'anonymous', getToken: noToken }

function usePublicOnly(): OwnerAuth {
  return publicOnly
}

function useClerkOwner(): OwnerAuth {
  const { isLoaded, isSignedIn, userId, getToken } = useAuth()
  // A fresh session token per request: Clerk tokens live about a minute.
  const token = useCallback<TokenGetter>(async () => (isSignedIn ? await getToken() : null), [isSignedIn, getToken])
  return {
    configured: true,
    loaded: isLoaded,
    signedIn: Boolean(isSignedIn),
    identity: isSignedIn && userId ? userId : 'anonymous',
    getToken: token,
  }
}

// The choice is fixed at build time, so hook order never changes.
export const useOwnerAuth: () => OwnerAuth = CLERK_KEY ? useClerkOwner : usePublicOnly
