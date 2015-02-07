'use strict';
var user = require("./../models/UserModel");
var user_repository = (function () {
    function user_repository() {
    }
    user_repository.prototype.updateUser = function (id, newName) {
        var usr = this.getUser(id);
        usr.NewName = newName;
    };
    user_repository.prototype.getUser = function (id) {
        var name = "Default";
        if (id === 1) {
            name = "Dio";
        }
        else if (id === 2) {
            name = "Annie";
        }
        return new user(id, name);
    };
    return user_repository;
})();
module.exports = user_repository;
//# sourceMappingURL=UserRepository.js.map