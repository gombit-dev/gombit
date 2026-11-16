import { useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { useNavigate } from "react-router";

import { bootstrapCSRF, useApiClient } from "../api/client";
import { applyContractErrors, fieldError } from "../api/formErrors";
import { unwrap } from "../api/generated/client";
import { setAuthenticated } from "../auth/session";

type LoginValues = {
  email: string;
  password: string;
  mfa_code: string;
};

const mfaPrompt = "Enter the code from your authenticator app, or one of your recovery codes.";

export function LoginPage() {
  const client = useApiClient();
  const navigate = useNavigate();
  const [status, setStatus] = useState("");
  // An account with multi-factor authentication answers the password with
  // a 422 on mfa_code: the code field appears, and the login is sent again
  // with it.
  const [mfaRequired, setMfaRequired] = useState(false);
  const {
    register,
    handleSubmit,
    setError,
    formState: { errors, isSubmitting },
  } = useForm<LoginValues>({
    defaultValues: { email: "", password: "", mfa_code: "" },
  });

  useEffect(() => {
    // Bootstrap the CSRF cookie so the first mutating request on this page
    // (register or login) has a token to double-submit.
    void bootstrapCSRF();
  }, []);

  async function onLogin(values: LoginValues) {
    setStatus("");
    try {
      // Mount effect starts CSRF; wait for that in-flight pair (or mint one
      // after clearSession) before POST /auth/login.
      await bootstrapCSRF();
      await unwrap(
        await client.POST("/api/v1/auth/login", {
          body: loginBody(values),
        }),
      );
      setAuthenticated(true);
      navigate("/");
    } catch (err: unknown) {
      if (fieldError(err, "mfa_code") !== undefined && !mfaRequired) {
        // The password was right: ask for the code (no error yet).
        setMfaRequired(true);
        return;
      }
      if (!applyContractErrors(setError, err)) {
        setStatus(err instanceof Error ? err.message : "login failed");
      }
    }
  }

  function loginBody(values: LoginValues) {
    const body = { email: values.email, password: values.password };
    return mfaRequired ? { ...body, mfa_code: values.mfa_code.trim() } : body;
  }

  async function onRegister(values: LoginValues) {
    setStatus("");
    try {
      await bootstrapCSRF();
      await unwrap(
        await client.POST("/api/v1/auth/register", {
          body: { email: values.email, password: values.password },
        }),
      );
      await onLogin(values);
    } catch (err: unknown) {
      if (!applyContractErrors(setError, err)) {
        setStatus(err instanceof Error ? err.message : "register failed");
      }
    }
  }

  return (
    <section>
      <h1>Sign in</h1>
      <p>Your session is an HttpOnly cookie. It is never exposed to page JavaScript.</p>
      <form
        onSubmit={handleSubmit(onLogin)}
      >
        <label>
          Email
          <input type="email" autoComplete="username" {...register("email", { required: "Email is required" })} />
        </label>
        {errors.email?.message ? <p>{errors.email.message}</p> : null}
        <label>
          Password
          <input type="password" autoComplete="current-password" {...register("password", { required: "Password is required" })} />
        </label>
        {errors.password?.message ? <p>{errors.password.message}</p> : null}
        {mfaRequired ? (
          <>
            <label>
              Authentication code
              <input autoComplete="one-time-code" autoFocus {...register("mfa_code", { required: "The code is required" })} />
            </label>
            <p>{errors.mfa_code?.message ?? mfaPrompt}</p>
          </>
        ) : null}
        <button type="submit" disabled={isSubmitting}>
          Log in
        </button>
        <button type="button" disabled={isSubmitting} onClick={handleSubmit(onRegister)}>
          Create account
        </button>
      </form>
      {status ? <p>{status}</p> : null}
    </section>
  );
}
