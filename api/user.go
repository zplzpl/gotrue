package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gofrs/uuid"
	"github.com/netlify/gotrue/models"
	"github.com/netlify/gotrue/storage"
)

// UserUpdateParams parameters for updating a user
type UserUpdateParams struct {
	Email    string                 `json:"email"`
	Password *string                `json:"password"`
	Data     map[string]interface{} `json:"data"`
	AppData  map[string]interface{} `json:"app_metadata,omitempty"`
	Phone    string                 `json:"phone"`
}

// UserGet returns a user
func (a *API) UserGet(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	claims := getClaims(ctx)
	if claims == nil {
		return badRequestError("Could not read claims")
	}

	userID, err := uuid.FromString(claims.Subject)
	if err != nil {
		return badRequestError("Could not read User ID claim")
	}

	aud := a.requestAud(ctx, r)
	if aud != claims.Audience {
		return badRequestError("Token audience doesn't match request audience")
	}

	user, err := models.FindUserByID(a.db, userID)
	if err != nil {
		if models.IsNotFoundError(err) {
			return notFoundError(err.Error())
		}
		return internalServerError("Database error finding user").WithInternalError(err)
	}

	return sendJSON(w, http.StatusOK, user)
}

// UserUpdate updates fields on a user
func (a *API) UserUpdate(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	config := a.getConfig(ctx)
	instanceID := getInstanceID(ctx)

	params := &UserUpdateParams{}
	jsonDecoder := json.NewDecoder(r.Body)
	err := jsonDecoder.Decode(params)
	if err != nil {
		return badRequestError("Could not read User Update params: %v", err)
	}

	claims := getClaims(ctx)
	userID, err := uuid.FromString(claims.Subject)
	if err != nil {
		return badRequestError("Could not read User ID claim")
	}

	user, err := models.FindUserByID(a.db, userID)
	if err != nil {
		if models.IsNotFoundError(err) {
			return notFoundError(err.Error())
		}
		return internalServerError("Database error finding user").WithInternalError(err)
	}

	log := getLogEntry(r)
	log.Debugf("Checking params for token %v", params)

	err = a.db.Transaction(func(tx *storage.Connection) error {
		var terr error
		if params.Password != nil {
			if len(*params.Password) < config.PasswordMinLength {
				return invalidPasswordLengthError(config)
			}

			if terr = user.UpdatePassword(tx, *params.Password); terr != nil {
				return internalServerError("Error during password storage").WithInternalError(terr)
			}
		}

		if params.Data != nil {
			if terr = user.UpdateUserMetaData(tx, params.Data); terr != nil {
				return internalServerError("Error updating user").WithInternalError(terr)
			}
		}

		if params.AppData != nil {
			if !a.isAdmin(ctx, user, config.JWT.Aud) {
				return unauthorizedError("Updating app_metadata requires admin privileges")
			}

			if terr = user.UpdateAppMetaData(tx, params.AppData); terr != nil {
				return internalServerError("Error updating user").WithInternalError(terr)
			}
		}

		if params.Email != "" && params.Email != user.GetEmail() {
			if terr = a.validateEmail(ctx, params.Email); terr != nil {
				return terr
			}

			var exists bool
			if exists, terr = models.IsDuplicatedEmail(tx, instanceID, params.Email, user.Aud); terr != nil {
				return internalServerError("Database error checking email").WithInternalError(terr)
			} else if exists {
				return unprocessableEntityError(DuplicateEmailMsg)
			}

			mailer := a.Mailer(ctx)
			referrer := a.getReferrer(r)
			if config.Mailer.SecureEmailChangeEnabled {
				if terr = a.sendSecureEmailChange(tx, user, mailer, params.Email, referrer); terr != nil {
					return internalServerError("Error sending change email").WithInternalError(terr)
				}
			} else {
				if terr = a.sendEmailChange(tx, user, mailer, params.Email, referrer); terr != nil {
					return internalServerError("Error sending change email").WithInternalError(terr)
				}
			}
		}

		if terr = models.NewAuditLogEntry(tx, instanceID, user, models.UserModifiedAction, nil); terr != nil {
			return internalServerError("Error recording audit log entry").WithInternalError(terr)
		}

		return nil
	})
	if err != nil {
		return err
	}

	return sendJSON(w, http.StatusOK, user)
}

type ResetPasswordParams struct {
	Password      string `json:"password"`
	RecoveryToken string `json:"recovery_token"`
}

// reset password
func (a *API) ResetPassword(w http.ResponseWriter, r *http.Request) error {

	ctx := r.Context()
	config := a.getConfig(ctx)
	instanceID := getInstanceID(ctx)

	params := &ResetPasswordParams{}
	jsonDecoder := json.NewDecoder(r.Body)
	err := jsonDecoder.Decode(params)
	if err != nil {
		return badRequestError("Could not read User Update params: %v", err)
	}

	if len(params.Password) < config.PasswordMinLength {
		return unprocessableEntityError(fmt.Sprintf("Password should be at least %d characters", config.PasswordMinLength))
	}

	claims := getClaims(ctx)
	userID, err := uuid.FromString(claims.Subject)
	if err != nil {
		return badRequestError("Could not read User ID claim")
	}

	user, err := models.FindUserByID(a.db, userID)
	if err != nil {
		if models.IsNotFoundError(err) {
			return notFoundError(err.Error())
		}
		return internalServerError("Database error finding user").WithInternalError(err)
	}

	if params.RecoveryToken != user.RecoveryToken || user.RecoverySentAt == nil {
		return badRequestError("Could not match your recovery token")
	}

	nextDay := user.RecoverySentAt.Add(24 * time.Hour)
	if time.Now().After(nextDay) {
		return expiredTokenError("Recovery token expired").WithInternalError(redirectWithQueryError)
	}

	err = a.db.Transaction(func(tx *storage.Connection) error {
		var terr error

		if terr = user.Recover(tx); terr != nil {
			return terr
		}

		if user.Authenticate(params.Password) {
			return unprocessableEntityError("The new password cannot be the same as the current password")
		}

		if terr = user.UpdatePassword(tx, params.Password); terr != nil {
			return internalServerError("Error during password storage").WithInternalError(terr)
		}

		if terr = models.NewAuditLogEntry(tx, instanceID, user, models.UserModifiedAction, nil); terr != nil {
			return internalServerError("Error recording audit log entry").WithInternalError(terr)
		}

		return nil
	})
	if err != nil {
		return err
	}

	return sendJSON(w, http.StatusOK, nil)
}

type ChangePasswordParams struct {
	CurrentPassword string `json:"current_password"`
	Password        string `json:"password"`
}

// change password
func (a *API) ChangePassword(w http.ResponseWriter, r *http.Request) error {

	ctx := r.Context()
	config := a.getConfig(ctx)
	instanceID := getInstanceID(ctx)

	params := &ChangePasswordParams{}
	jsonDecoder := json.NewDecoder(r.Body)
	err := jsonDecoder.Decode(params)
	if err != nil {
		return badRequestError("Could not read User Update params: %v", err)
	}

	claims := getClaims(ctx)
	userID, err := uuid.FromString(claims.Subject)
	if err != nil {
		return badRequestError("Could not read User ID claim")
	}

	user, err := models.FindUserByID(a.db, userID)
	if err != nil {
		if models.IsNotFoundError(err) {
			return notFoundError(err.Error())
		}
		return internalServerError("Database error finding user").WithInternalError(err)
	}

	if !user.Authenticate(params.CurrentPassword) {
		return badRequestError("Wrong password, please try again.")
	}

	err = a.db.Transaction(func(tx *storage.Connection) error {
		var terr error
		if params.Password != "" {
			if len(params.Password) < config.PasswordMinLength {
				return unprocessableEntityError(fmt.Sprintf("Password should be at least %d characters", config.PasswordMinLength))
			}

			if terr = user.UpdatePassword(tx, params.Password); terr != nil {
				return internalServerError("Error during password storage").WithInternalError(terr)
			}
		}

		if terr = models.NewAuditLogEntry(tx, instanceID, user, models.UserModifiedAction, nil); terr != nil {
			return internalServerError("Error recording audit log entry").WithInternalError(terr)
		}

		return nil
	})
	if err != nil {
		return err
	}

	return sendJSON(w, http.StatusOK, nil)
}

func (a *API) ResendConfirm(w http.ResponseWriter, r *http.Request) error {

	ctx := r.Context()
	config := a.getConfig(ctx)

	claims := getClaims(ctx)
	userID, err := uuid.FromString(claims.Subject)
	if err != nil {
		return badRequestError("Could not read User ID claim")
	}

	user, err := models.FindUserByID(a.db, userID)
	if err != nil {
		if models.IsNotFoundError(err) {
			return notFoundError(err.Error())
		}
		return internalServerError("Database error finding user").WithInternalError(err)
	}

	if user.IsConfirmed() {
		return badRequestError("Email Confirmed")
	}

	err = a.db.Transaction(func(tx *storage.Connection) error {

		var terr error

		mailer := a.Mailer(ctx)
		referrer := a.getReferrer(r)
		if terr = sendConfirmation(tx, user, mailer, config.SMTP.MaxFrequency, referrer); terr != nil {
			return internalServerError("Error sending confirmation mail").WithInternalError(terr)
		}

		return nil
	})

	if err != nil {
		return err
	}

	return nil
}
