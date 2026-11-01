import { useState } from "react";
import { useForm, Controller } from "react-hook-form";
import { useNavigate } from "react-router";

import {
  Alert,
  Box,
  Button,
  CircularProgress,
  Paper,
  TextField,
  Typography,
} from "@mui/material";

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
    control,
    handleSubmit,
    setError,
    formState: { isSubmitting },
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
    <Box
      sx={{
        minHeight: "100vh",
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        p: 2,
      }}
    >
      <Paper sx={{ p: 4, width: "100%", maxWidth: 420 }}>
        <Typography component="h1" variant="h5" sx={{ mb: 1 }}>
          Sign in
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 3 }}>
          Access tokens stay in memory. They are never written to web storage.
          </Typography>
        {status ? (
          <Alert severity="error" sx={{ mb: 2 }}>
            {status}
          </Alert>
        ) : null}
        <Box
          component="form"
          onSubmit={handleSubmit(onLogin)}
          sx={{ display: "flex", flexDirection: "column", gap: 2 }}
        >
          <Controller
            name="email"
            control={control}
            rules={{ required: "Email is required" }}
            render={({ field, fieldState }) => (
              <TextField
                {...field}
                type="email"
                label="Email"
                autoComplete="username"
                fullWidth
                error={!!fieldState.error}
                helperText={fieldState.error?.message}
                disabled={isSubmitting}
              />
            )}
          />
          <Controller
            name="password"
            control={control}
            rules={{ required: "Password is required" }}
            render={({ field, fieldState }) => (
              <TextField
                {...field}
                type="password"
                label="Password"
                autoComplete="current-password"
                fullWidth
                error={!!fieldState.error}
                helperText={fieldState.error?.message}
                disabled={isSubmitting}
              />
            )}
          />
          <Button type="submit" variant="contained" disabled={isSubmitting}>
            {isSubmitting ? <CircularProgress size={24} color="inherit" /> : "Log in"}
          </Button>
          <Button
            type="button"
            variant="outlined"
            disabled={isSubmitting}
            onClick={handleSubmit(onRegister)}
          >
            Create account
          </Button>
        </Box>
      </Paper>
    </Box>
  );
}
