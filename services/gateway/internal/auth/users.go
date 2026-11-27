package auth

import "github.com/Yulian302/deliverit/platform/identity"

// User is a demo identity someone can log in as.
type User struct {
	ID   string
	Name string
	Role identity.Role
	// RestaurantID is set only for restaurant staff (a seeded restaurant).
	RestaurantID string
}

// demoUsers are fixed so demos, curl examples and tests are repeatable.
// IDs share a prefix with the seeded restaurants' (0193a1b0-…) but use
// 0193a1b1-…, and end in a hex tag: c = customer, a = restaurant staff
// (a1 → restaurant …01, a5 → …05), b = courier, d = ops.
var demoUsers = []User{
	{ID: "0193a1b1-0000-7000-8000-0000000000c1", Name: "Olena", Role: identity.RoleCustomer},
	{ID: "0193a1b1-0000-7000-8000-0000000000c2", Name: "Taras", Role: identity.RoleCustomer},
	{ID: "0193a1b1-0000-7000-8000-0000000000a1", Name: "Khreshchatyk Kitchen staff", Role: identity.RoleRestaurant, RestaurantID: "0193a1b0-0000-7000-8000-000000000001"},
	{ID: "0193a1b1-0000-7000-8000-0000000000a5", Name: "Podil Pierogi staff", Role: identity.RoleRestaurant, RestaurantID: "0193a1b0-0000-7000-8000-000000000005"},
	{ID: "0193a1b1-0000-7000-8000-0000000000b1", Name: "Courier Andrii", Role: identity.RoleCourier},
	{ID: "0193a1b1-0000-7000-8000-0000000000b2", Name: "Courier Iryna", Role: identity.RoleCourier},
	{ID: "0193a1b1-0000-7000-8000-0000000000d1", Name: "Ops desk", Role: identity.RoleOps},
}

// DemoUsers returns a copy of the demo users.
func DemoUsers() []User {
	return append([]User(nil), demoUsers...)
}

// FindUser looks a demo user up by ID.
func FindUser(id string) (User, bool) {
	for _, u := range demoUsers {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}
