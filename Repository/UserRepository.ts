'use strict';
import user = require("./../models/UserModel");
class user_repository {
  constructor() {

  }

  public updateUser(id:number, newName:string):void {
    var usr = this.getUser(id);

    usr.NewName = newName;
  }


  public getUser(id:number):user {
    var name = "Default";
    if (id === 1) {
      name = "Dio";
    }
    else if (id === 2) {
      name = "Annie";
    }
    return new user(id, name);
  }
}


export = user_repository;