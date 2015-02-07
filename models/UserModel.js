'use strict';
var User = (function () {
    function User(id, name) {
        this._id = id;
        this._name = name;
    }
    Object.defineProperty(User.prototype, "Name", {
        get: function () {
            return this._name;
        },
        set: function (newName) {
            this._name = newName;
        },
        enumerable: true,
        configurable: true
    });
    Object.defineProperty(User.prototype, "NewName", {
        get: function () {
            return this._newName;
        },
        set: function (newName) {
            this._newName = newName;
        },
        enumerable: true,
        configurable: true
    });
    Object.defineProperty(User.prototype, "Id", {
        get: function () {
            return this._id;
        },
        set: function (newId) {
            this._id = newId;
        },
        enumerable: true,
        configurable: true
    });
    return User;
})();
module.exports = User;
//# sourceMappingURL=UserModel.js.map