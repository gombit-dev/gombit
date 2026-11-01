import { useState } from "react";
import { useForm } from "react-hook-form";
import { useNavigate } from "react-router";

import { useApiClient } from "../api/client";
import { applyContractErrors } from "../api/formErrors";
import { unwrap } from "../api/generated/client";
import { applyTokenPair } from "../auth/session";

type LoginValues = {
  email: string;
  password: string;
};

export function LoginPage() {
  const client = useApiClient();
  const navigate = useNavigate();
  const [status, setStatus] = useState("");
  const {
    register,
    handleSubmit,
    setError,
    formState: { errors, isSubmitting },
  } = useForm<LoginValues>({
    defaultValues: { email: "", password: "" },
  });

  async function onLogin(values: LoginValues) {
    setStatus("");
    try {
      const result = await unwrap(
        await client.POST("/api/v1/auth/login", {
          body: { email: values.email, password: values.password },
        }),
      );
      applyTokenPair(result.data);
      navigate("/");
    } catch (err: unknown) {
      if (!applyContractErrors(setError, err)) {
        setStatus(err instanceof Error ? err.message : "login failed");
      }
    }
  }

  async function onRegister(values: LoginValues) {
    setStatus("");
    try {
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
      <p>Access tokens stay in memory. They are never written to web storage.</p>
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
